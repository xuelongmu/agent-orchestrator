// Package hooksjson implements the matcher-group hooks file that several agents
// (claude-code, goose, qwen, agy, droid) share byte-for-byte in shape. Each such
// file is a JSON object with a "hooks" sub-map keyed by native event name, whose
// values are matcher groups ({matcher?, hooks:[{type,command,timeout}]}). The
// adapters differed only in the file path, the AO command prefix, the per-hook
// timeout, and which events they install, so they describe those with a Manager
// and share the install/uninstall/detect logic here.
//
// The read/write path preserves every top-level key and every user-defined hook
// AO does not own, and writes atomically, so installing AO's hooks never clobbers
// unrelated settings.
package hooksjson

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/hookutil"
)

// HookEntry is one command hook inside a matcher group.
type HookEntry struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`

	raw json.RawMessage
}

// MatcherGroup is a set of hooks sharing one matcher. Matcher is a pointer so it
// round-trips exactly: events that require a matcher (e.g. claude SessionStart's
// "startup") carry one; events that omit it serialize without the key.
type MatcherGroup struct {
	Matcher *string     `json:"matcher,omitempty"`
	Hooks   []HookEntry `json:"hooks"`

	raw   json.RawMessage
	valid bool

	matcherChanged bool
}

// HookSpec describes one hook AO installs: the native event it attaches to, its
// optional matcher, and the command to run. Adapters define these in code rather
// than reading an embedded template.
type HookSpec struct {
	Event   string
	Matcher *string
	Command string
}

// Manager installs, removes, and detects AO's hooks in one agent's matcher-group
// hooks file. Construct one per adapter with its file path, command prefix,
// per-hook timeout, and managed hook set.
type Manager struct {
	// Label prefixes error messages, e.g. "claude-code" or "goose", so the
	// wrapped error reads "<label>.GetAgentHooks: ...".
	Label string
	// CommandPrefix identifies AO-owned hook commands, e.g. "ao hooks goose ".
	// Install skips commands already present and uninstall/detect match on it.
	CommandPrefix string
	// Timeout is written into each installed hook entry.
	Timeout int
	// Path returns the hooks file path for a workspace.
	Path func(workspacePath string) string
	// Managed is the set of hooks AO installs.
	Managed []HookSpec
}

// Install merges AO's managed hooks into the workspace's hooks file, preserving
// user-defined hooks and unrelated settings, and is idempotent (a command
// already present is not appended). It also writes a self-ignoring .gitignore
// covering the hooks file so it does not block worktree teardown.
func (m Manager) Install(ctx context.Context, workspacePath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(workspacePath) == "" {
		return fmt.Errorf("%s.GetAgentHooks: WorkspacePath is required", m.Label)
	}

	hooksPath := m.Path(workspacePath)
	topLevel, rawHooks, err := readHooksFile(hooksPath)
	if err != nil {
		return fmt.Errorf("%s.GetAgentHooks: %w", m.Label, err)
	}
	if err := m.normalizeLegacyEvents(rawHooks); err != nil {
		return fmt.Errorf("%s.GetAgentHooks: %w", m.Label, err)
	}

	for event, specs := range m.groupByEvent() {
		var groups []MatcherGroup
		if err := parseEvent(rawHooks, event, &groups); err != nil {
			return fmt.Errorf("%s.GetAgentHooks: %w", m.Label, err)
		}
		for _, spec := range specs {
			entry := HookEntry{Type: "command", Command: spec.Command, Timeout: m.Timeout}
			groups = ensureHookMatcher(groups, entry, spec.Matcher)
		}
		if err := marshalEvent(rawHooks, event, groups); err != nil {
			return fmt.Errorf("%s.GetAgentHooks: %w", m.Label, err)
		}
	}

	if err := writeHooksFile(hooksPath, topLevel, rawHooks); err != nil {
		return fmt.Errorf("%s.GetAgentHooks: %w", m.Label, err)
	}
	if err := hookutil.EnsureWorkspaceGitignore(filepath.Dir(hooksPath), filepath.Base(hooksPath)); err != nil {
		return fmt.Errorf("%s.GetAgentHooks: gitignore: %w", m.Label, err)
	}
	return nil
}

// Uninstall removes AO's hooks from the workspace's hooks file, leaving
// user-defined hooks and unrelated settings untouched. A missing file is a no-op.
func (m Manager) Uninstall(ctx context.Context, workspacePath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(workspacePath) == "" {
		return fmt.Errorf("%s.UninstallHooks: workspacePath is required", m.Label)
	}

	hooksPath := m.Path(workspacePath)
	if _, err := os.Stat(hooksPath); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	topLevel, rawHooks, err := readHooksFile(hooksPath)
	if err != nil {
		return fmt.Errorf("%s.UninstallHooks: %w", m.Label, err)
	}

	for _, event := range m.managedEvents() {
		var groups []MatcherGroup
		if err := parseEvent(rawHooks, event, &groups); err != nil {
			return fmt.Errorf("%s.UninstallHooks: %w", m.Label, err)
		}
		normalizeLegacyGroups(groups, m.legacyMatcher(event), m.CommandPrefix)
		groups = removeManaged(groups, m.CommandPrefix)
		if err := marshalEvent(rawHooks, event, groups); err != nil {
			return fmt.Errorf("%s.UninstallHooks: %w", m.Label, err)
		}
	}

	if err := writeHooksFile(hooksPath, topLevel, rawHooks); err != nil {
		return fmt.Errorf("%s.UninstallHooks: %w", m.Label, err)
	}
	return nil
}

// AreInstalled reports whether any AO hook is present in the workspace's hooks
// file. A missing file means none are installed.
func (m Manager) AreInstalled(ctx context.Context, workspacePath string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if strings.TrimSpace(workspacePath) == "" {
		return false, fmt.Errorf("%s.AreHooksInstalled: workspacePath is required", m.Label)
	}

	hooksPath := m.Path(workspacePath)
	if _, err := os.Stat(hooksPath); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	_, rawHooks, err := readHooksFile(hooksPath)
	if err != nil {
		return false, fmt.Errorf("%s.AreHooksInstalled: %w", m.Label, err)
	}

	for _, event := range m.managedEvents() {
		var groups []MatcherGroup
		if err := parseEvent(rawHooks, event, &groups); err != nil {
			return false, fmt.Errorf("%s.AreHooksInstalled: %w", m.Label, err)
		}
		normalizeLegacyGroups(groups, m.legacyMatcher(event), m.CommandPrefix)
		for _, group := range groups {
			for _, hook := range group.Hooks {
				if strings.HasPrefix(hook.Command, m.CommandPrefix) {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// groupByEvent groups the managed specs by event so each event array is
// rewritten once.
func (m Manager) groupByEvent() map[string][]HookSpec {
	byEvent := map[string][]HookSpec{}
	for _, spec := range m.Managed {
		byEvent[spec.Event] = append(byEvent[spec.Event], spec)
	}
	return byEvent
}

// managedEvents returns the distinct managed events, in first-seen order.
func (m Manager) managedEvents() []string {
	seen := map[string]bool{}
	events := make([]string, 0, len(m.Managed))
	for _, spec := range m.Managed {
		if !seen[spec.Event] {
			seen[spec.Event] = true
			events = append(events, spec.Event)
		}
	}
	return events
}

// legacyMatcher returns the matcher used when wrapping a bare command for an
// event. A configured matcher takes precedence (notably SessionStart's
// "startup"); events without one use the legacy format's empty matcher.
func (m Manager) legacyMatcher(event string) *string {
	matcher := ""
	for _, spec := range m.Managed {
		if spec.Event == event && spec.Matcher != nil {
			matcher = *spec.Matcher
			break
		}
	}
	return &matcher
}

// readHooksFile loads the file into a top-level raw map plus the decoded "hooks"
// sub-map, preserving every key AO doesn't manage. A missing or empty file
// yields empty maps.
func readHooksFile(hooksPath string) (topLevel, rawHooks map[string]json.RawMessage, err error) {
	topLevel = map[string]json.RawMessage{}
	rawHooks = map[string]json.RawMessage{}

	data, err := os.ReadFile(hooksPath) //nolint:gosec // path built from caller-owned workspace dir
	if errors.Is(err, os.ErrNotExist) {
		return topLevel, rawHooks, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", hooksPath, err)
	}
	if strings.TrimSpace(string(data)) == "" {
		return topLevel, rawHooks, nil
	}
	if err := json.Unmarshal(data, &topLevel); err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", hooksPath, err)
	}
	if hooksRaw, ok := topLevel["hooks"]; ok {
		if err := json.Unmarshal(hooksRaw, &rawHooks); err != nil {
			return nil, nil, fmt.Errorf("parse hooks in %s: %w", hooksPath, err)
		}
		if rawHooks == nil {
			rawHooks = map[string]json.RawMessage{}
		}
	}
	return topLevel, rawHooks, nil
}

// writeHooksFile folds rawHooks back into topLevel and writes the file
// atomically. An empty hooks map drops the "hooks" key entirely.
func writeHooksFile(hooksPath string, topLevel, rawHooks map[string]json.RawMessage) error {
	if len(rawHooks) == 0 {
		delete(topLevel, "hooks")
	} else {
		hooksJSON, err := json.Marshal(rawHooks)
		if err != nil {
			return fmt.Errorf("encode hooks: %w", err)
		}
		topLevel["hooks"] = hooksJSON
	}

	if err := os.MkdirAll(filepath.Dir(hooksPath), 0o750); err != nil {
		return fmt.Errorf("create hook dir: %w", err)
	}
	data, err := json.MarshalIndent(topLevel, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", hooksPath, err)
	}
	data = append(data, '\n')
	if err := hookutil.AtomicWriteFile(hooksPath, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", hooksPath, err)
	}
	return nil
}

func parseEvent(rawHooks map[string]json.RawMessage, event string, target *[]MatcherGroup) error {
	data, ok := rawHooks[event]
	if !ok {
		return nil
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return fmt.Errorf("parse %s hooks: expected array", event)
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("parse %s hooks: %w", event, err)
	}
	return nil
}

// normalizeLegacyEvents repairs AO-owned bare command entries in every valid
// event array, not just events AO manages. User commands and malformed event
// values stay opaque; a managed malformed event will still produce a normal
// parse error when AO attempts to install its hook into that event.
func (m Manager) normalizeLegacyEvents(rawHooks map[string]json.RawMessage) error {
	for event, data := range rawHooks {
		trimmed := bytes.TrimSpace(data)
		if len(trimmed) == 0 || trimmed[0] != '[' {
			continue
		}
		var groups []MatcherGroup
		if err := json.Unmarshal(data, &groups); err != nil {
			continue
		}
		changed := normalizeLegacyGroups(groups, m.legacyMatcher(event), m.CommandPrefix)
		if changed {
			if err := marshalEvent(rawHooks, event, groups); err != nil {
				return err
			}
		}
	}
	return nil
}

func normalizeLegacyGroups(groups []MatcherGroup, matcher *string, commandPrefix string) bool {
	changed := false
	for i := range groups {
		changed = groups[i].normalizeLegacyCommand(matcher, commandPrefix) || changed
	}
	return changed
}

func marshalEvent(rawHooks map[string]json.RawMessage, event string, groups []MatcherGroup) error {
	if len(groups) == 0 {
		delete(rawHooks, event)
		return nil
	}
	data, err := json.Marshal(groups)
	if err != nil {
		return fmt.Errorf("encode %s hooks: %w", event, err)
	}
	rawHooks[event] = data
	return nil
}

// addHook appends hook to the group with a matching matcher, creating that group
// if none matches.
func addHook(groups []MatcherGroup, hook HookEntry, matcher *string) []MatcherGroup {
	for i, group := range groups {
		if group.isMatcherGroup() && matchersEqual(group.Matcher, matcher) {
			groups[i].Hooks = append(groups[i].Hooks, hook)
			return groups
		}
	}
	return append(groups, MatcherGroup{Matcher: matcher, Hooks: []HookEntry{hook}, valid: true})
}

// ensureHookMatcher keeps an existing managed command when it is already under
// the configured matcher. If it exists under another matcher (including a
// normalized legacy bare command's empty matcher), it moves that entry rather
// than duplicating it, retaining any unknown fields on the original entry.
func ensureHookMatcher(groups []MatcherGroup, hook HookEntry, matcher *string) []MatcherGroup {
	canonicalGroup, canonicalHook := -1, -1
	canonicalMatches := false
	for groupIndex, group := range groups {
		if !group.isMatcherGroup() {
			continue
		}
		for hookIndex, existing := range group.Hooks {
			if existing.Command != hook.Command {
				continue
			}
			matches := matchersEqual(group.Matcher, matcher)
			if canonicalGroup == -1 || matches && !canonicalMatches {
				canonicalGroup, canonicalHook = groupIndex, hookIndex
				canonicalMatches = matches
			}
		}
	}
	if canonicalGroup == -1 {
		return addHook(groups, hook, matcher)
	}

	canonicalEntry := groups[canonicalGroup].Hooks[canonicalHook]
	result := make([]MatcherGroup, 0, len(groups))
	canonicalResultGroup := -1
	for groupIndex, group := range groups {
		if !group.isMatcherGroup() {
			result = append(result, group)
			continue
		}
		kept := make([]HookEntry, 0, len(group.Hooks))
		for hookIndex, existing := range group.Hooks {
			if existing.Command != hook.Command || groupIndex == canonicalGroup && hookIndex == canonicalHook {
				kept = append(kept, existing)
			}
		}
		if len(kept) == 0 {
			continue
		}
		group.Hooks = kept
		if groupIndex == canonicalGroup {
			canonicalResultGroup = len(result)
		}
		result = append(result, group)
	}

	if canonicalMatches {
		return result
	}
	if len(result[canonicalResultGroup].Hooks) == 1 {
		result[canonicalResultGroup].Matcher = matcher
		result[canonicalResultGroup].matcherChanged = true
		return result
	}

	group := &result[canonicalResultGroup]
	for hookIndex, existing := range group.Hooks {
		if existing.Command == hook.Command {
			group.Hooks = append(group.Hooks[:hookIndex], group.Hooks[hookIndex+1:]...)
			break
		}
	}
	return addHook(result, canonicalEntry, matcher)
}

// removeManaged strips AO hook entries (matched by command prefix) from every
// group, dropping any group left without hooks so the event array doesn't
// accumulate empty matcher objects.
func removeManaged(groups []MatcherGroup, prefix string) []MatcherGroup {
	result := make([]MatcherGroup, 0, len(groups))
	for _, group := range groups {
		if !group.isMatcherGroup() {
			result = append(result, group)
			continue
		}
		kept := make([]HookEntry, 0, len(group.Hooks))
		for _, hook := range group.Hooks {
			if !strings.HasPrefix(hook.Command, prefix) {
				kept = append(kept, hook)
			}
		}
		if len(kept) > 0 {
			group.Hooks = kept
			result = append(result, group)
		}
	}
	return result
}

func matchersEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// UnmarshalJSON retains the original hook entry so fields unknown to AO survive
// an install. Hook entries are never edited in place: AO either keeps/removes an
// existing entry or appends a newly constructed one.
func (h *HookEntry) UnmarshalJSON(data []byte) error {
	h.Type = ""
	h.Command = ""
	h.Timeout = 0
	h.raw = append(h.raw[:0], data...)

	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return fmt.Errorf("decode hook entry: %w", err)
	}
	if fields == nil {
		return nil
	}
	_ = json.Unmarshal(fields["type"], &h.Type)
	_ = json.Unmarshal(fields["command"], &h.Command)
	_ = json.Unmarshal(fields["timeout"], &h.Timeout)
	return nil
}

// MarshalJSON returns existing entries verbatim at the JSON-value level. New
// AO entries have no raw representation and use the public wire fields.
func (h HookEntry) MarshalJSON() ([]byte, error) {
	if h.raw != nil {
		return h.raw, nil
	}
	type wire HookEntry
	return json.Marshal(wire(h))
}

// UnmarshalJSON distinguishes valid matcher groups from other array values.
// Invalid and non-command values are retained as opaque JSON rather than being
// coerced into a zero-value group and subsequently written as {"hooks":null}.
func (g *MatcherGroup) UnmarshalJSON(data []byte) error {
	g.Matcher = nil
	g.Hooks = nil
	g.raw = append(g.raw[:0], data...)
	g.valid = false
	g.matcherChanged = false

	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return fmt.Errorf("decode matcher group: %w", err)
	}
	if fields == nil {
		return nil
	}
	hooksRaw, ok := fields["hooks"]
	if !ok {
		return nil
	}
	if hooksRaw = bytes.TrimSpace(hooksRaw); len(hooksRaw) == 0 || hooksRaw[0] != '[' {
		return nil
	}
	var hooks []HookEntry
	if err := json.Unmarshal(hooksRaw, &hooks); err != nil {
		return fmt.Errorf("decode matcher group hooks: %w", err)
	}

	var matcher *string
	if matcherRaw, ok := fields["matcher"]; ok {
		matcherRaw = bytes.TrimSpace(matcherRaw)
		if string(matcherRaw) != "null" {
			if len(matcherRaw) == 0 || matcherRaw[0] != '"' {
				return nil
			}
			var value string
			if err := json.Unmarshal(matcherRaw, &value); err != nil {
				return fmt.Errorf("decode matcher group matcher: %w", err)
			}
			matcher = &value
		}
	}
	g.Matcher = matcher
	g.Hooks = hooks
	g.valid = true
	return nil
}

// MarshalJSON overlays the possibly updated hook list on a parsed matcher
// group, preserving unknown group fields. Opaque invalid values are emitted
// unchanged; newly created groups use the public wire fields.
func (g MatcherGroup) MarshalJSON() ([]byte, error) {
	if !g.valid {
		if g.raw != nil {
			return g.raw, nil
		}
		return marshalNewMatcherGroup(g)
	}
	if g.raw == nil {
		return marshalNewMatcherGroup(g)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(g.raw, &fields); err != nil {
		return nil, err
	}
	hooks, err := json.Marshal(g.Hooks)
	if err != nil {
		return nil, err
	}
	fields["hooks"] = hooks
	if g.matcherChanged {
		if g.Matcher == nil {
			delete(fields, "matcher")
		} else {
			matcher, err := json.Marshal(*g.Matcher)
			if err != nil {
				return nil, err
			}
			fields["matcher"] = matcher
		}
	}
	return json.Marshal(fields)
}

func marshalNewMatcherGroup(g MatcherGroup) ([]byte, error) {
	fields := map[string]any{"hooks": g.Hooks}
	if g.Matcher != nil {
		fields["matcher"] = *g.Matcher
	}
	return json.Marshal(fields)
}

func (g MatcherGroup) isMatcherGroup() bool {
	return g.valid || g.raw == nil
}

// normalizeLegacyCommand wraps an old AO-owned bare command in a matcher group.
// Its original JSON becomes the group's sole hook entry, so custom fields are
// retained. User, malformed, and non-command array values remain opaque.
func (g *MatcherGroup) normalizeLegacyCommand(defaultMatcher *string, commandPrefix string) bool {
	if g.valid || g.raw == nil {
		return false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(g.raw, &fields); err != nil || fields == nil {
		return false
	}
	if _, hasHooks := fields["hooks"]; hasHooks {
		return false
	}
	var hookType, command string
	if err := json.Unmarshal(fields["type"], &hookType); err != nil || hookType != "command" {
		return false
	}
	if err := json.Unmarshal(fields["command"], &command); err != nil {
		return false
	}
	if !strings.HasPrefix(command, commandPrefix) {
		return false
	}

	matcher := ""
	if defaultMatcher != nil {
		matcher = *defaultMatcher
	}
	if matcherRaw, ok := fields["matcher"]; ok {
		matcherRaw = bytes.TrimSpace(matcherRaw)
		if len(matcherRaw) == 0 || matcherRaw[0] != '"' {
			return false
		}
		if err := json.Unmarshal(matcherRaw, &matcher); err != nil {
			return false
		}
	}
	var hook HookEntry
	if err := json.Unmarshal(g.raw, &hook); err != nil {
		return false
	}
	g.raw = nil
	g.Matcher = &matcher
	g.Hooks = []HookEntry{hook}
	g.valid = true
	return true
}
