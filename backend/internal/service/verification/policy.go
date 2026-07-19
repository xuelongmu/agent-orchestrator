package verification

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Command is one operator-approved, argv-based verification profile.
type Command struct {
	Argv             []string `json:"argv"`
	WorkingDirectory string   `json:"workingDirectory,omitempty"`
	TimeoutSeconds   int      `json:"timeoutSeconds,omitempty"`
}

// Policy is loaded once at daemon startup from an operator-owned file. It is
// deliberately separate from worker-reachable project configuration routes.
type Policy struct {
	Profiles map[string]Command            `json:"profiles,omitempty"`
	Projects map[string]map[string]Command `json:"projects,omitempty"`
}

var builtInProfiles = map[string]Command{
	"backend":  {Argv: []string{"go", "test", "./..."}, WorkingDirectory: "backend"},
	"frontend": {Argv: []string{"npm", "test", "--", "--run"}, WorkingDirectory: "frontend"},
}

// LoadPolicy loads and validates an immutable startup policy. An empty path
// enables only the compiled backend/frontend profiles.
func LoadPolicy(path string) (Policy, error) {
	policy := Policy{}.withDefaults()
	if strings.TrimSpace(path) == "" {
		return policy, nil
	}
	if !filepath.IsAbs(path) {
		return Policy{}, fmt.Errorf("AO_VERIFY_CONFIG_FILE must be absolute")
	}
	file, err := os.Open(path)
	if err != nil {
		return Policy{}, fmt.Errorf("open verification policy: %w", err)
	}
	defer func() { _ = file.Close() }()
	var configured Policy
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&configured); err != nil {
		return Policy{}, fmt.Errorf("decode verification policy: %w", err)
	}
	for name, command := range configured.Profiles {
		policy.Profiles[name] = cloneCommand(command)
	}
	for project, profiles := range configured.Projects {
		if strings.TrimSpace(project) == "" {
			return Policy{}, fmt.Errorf("verification project id must not be empty")
		}
		copyProfiles := make(map[string]Command, len(profiles))
		for name, command := range profiles {
			copyProfiles[name] = cloneCommand(command)
		}
		policy.Projects[project] = copyProfiles
	}
	if err := policy.Validate(); err != nil {
		return Policy{}, err
	}
	return policy, nil
}

// Validate rejects unsafe or malformed operator profiles before the daemon
// begins serving. Commands are always direct argv executions; known shells are
// refused even in operator configuration.
func (p Policy) Validate() error {
	for scope, profiles := range p.scopes() {
		for name, command := range profiles {
			if err := validateProfileName(name); err != nil {
				return fmt.Errorf("%s: %w", scope, err)
			}
			if err := validateCommand(command); err != nil {
				return fmt.Errorf("%s.%s: %w", scope, name, err)
			}
		}
	}
	return nil
}

// Resolve selects a project override first, then a global profile.
func (p Policy) Resolve(project domain.ProjectID, name string) (Command, bool) {
	if profiles := p.Projects[string(project)]; profiles != nil {
		if command, ok := profiles[name]; ok {
			return cloneCommand(command), true
		}
	}
	command, ok := p.Profiles[name]
	return cloneCommand(command), ok
}

// Allowed returns the sorted profile names available to a project.
func (p Policy) Allowed(project domain.ProjectID) []string {
	seen := make(map[string]struct{}, len(p.Profiles))
	for name := range p.Profiles {
		seen[name] = struct{}{}
	}
	for name := range p.Projects[string(project)] {
		seen[name] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (p Policy) withDefaults() Policy {
	out := Policy{Profiles: make(map[string]Command, len(builtInProfiles)+len(p.Profiles)), Projects: make(map[string]map[string]Command, len(p.Projects))}
	for name, command := range builtInProfiles {
		out.Profiles[name] = cloneCommand(command)
	}
	for name, command := range p.Profiles {
		out.Profiles[name] = cloneCommand(command)
	}
	for project, profiles := range p.Projects {
		out.Projects[project] = make(map[string]Command, len(profiles))
		for name, command := range profiles {
			out.Projects[project][name] = cloneCommand(command)
		}
	}
	return out
}

func (p Policy) scopes() map[string]map[string]Command {
	scopes := map[string]map[string]Command{"profiles": p.Profiles}
	for project, profiles := range p.Projects {
		scopes["projects."+project] = profiles
	}
	return scopes
}

func validateProfileName(name string) error {
	if name == "" || len(name) > 64 {
		return fmt.Errorf("profile name %q must be 1-64 characters", name)
	}
	for _, char := range name {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-' || char == '_' || char == '.' {
			continue
		}
		return fmt.Errorf("profile name %q must use lowercase letters, numbers, '.', '-', or '_'", name)
	}
	return nil
}

func validateCommand(command Command) error {
	if len(command.Argv) == 0 || strings.TrimSpace(command.Argv[0]) == "" {
		return fmt.Errorf("argv must contain an executable")
	}
	for index, arg := range command.Argv {
		if strings.ContainsRune(arg, '\x00') {
			return fmt.Errorf("argv[%d] must not contain NUL", index)
		}
	}
	executable := strings.ToLower(filepath.Base(command.Argv[0]))
	executable = strings.TrimSuffix(executable, filepath.Ext(executable))
	if _, shell := map[string]struct{}{"sh": {}, "bash": {}, "dash": {}, "zsh": {}, "fish": {}, "ksh": {}, "cmd": {}, "powershell": {}, "pwsh": {}, "wscript": {}, "cscript": {}, "mshta": {}, "osascript": {}}[executable]; shell {
		return fmt.Errorf("shell executable %q is not allowed", command.Argv[0])
	}
	if err := validateRelativeDirectory(command.WorkingDirectory); err != nil {
		return err
	}
	if command.TimeoutSeconds < 0 || command.TimeoutSeconds > 3600 {
		return fmt.Errorf("timeoutSeconds must be between 0 and 3600")
	}
	return nil
}

func validateRelativeDirectory(path string) error {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return nil
	}
	if filepath.IsAbs(trimmed) || strings.HasPrefix(trimmed, "/") || strings.HasPrefix(trimmed, `\`) {
		return fmt.Errorf("workingDirectory must be workspace-relative")
	}
	clean := filepath.Clean(trimmed)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("workingDirectory must not escape the workspace")
	}
	for _, segment := range strings.Split(filepath.ToSlash(clean), "/") {
		if segment == ".." {
			return fmt.Errorf("workingDirectory must not escape the workspace")
		}
	}
	return nil
}

func cloneCommand(command Command) Command {
	command.Argv = append([]string(nil), command.Argv...)
	return command
}
