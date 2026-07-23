package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/claudecode"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/codex"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
)

type providerDelivery struct {
	record      domain.SessionRecord
	environment map[string]string
	restore     ports.RestoreConfig
	finalCheck  func(context.Context) error
	message     string
}

// providerSendFunc submits one message through a provider-owned structured
// protocol. accepted is the at-most-once boundary: callers may use the legacy
// terminal fallback only when it is false.
type providerSendFunc func(context.Context, providerDelivery) (accepted bool, err error)
type providerEnvironmentFunc func(context.Context, domain.SessionID) (map[string]string, error)
type providerRestoreConfigFunc func(context.Context, domain.SessionID) (ports.RestoreConfig, error)

type providerMessenger struct {
	fallback      runtimeMessenger
	providers     map[domain.AgentHarness]providerSendFunc
	environment   providerEnvironmentFunc
	restoreConfig providerRestoreConfigFunc
	logger        *slog.Logger
}

const providerAcknowledgementTimeout = 15 * time.Second

var errProviderSessionChanged = errors.New("session changed during structured provider handshake")

func newProviderMessenger(fallback runtimeMessenger, logger *slog.Logger) *providerMessenger {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &providerMessenger{
		fallback: fallback,
		logger:   logger,
		providers: map[domain.AgentHarness]providerSendFunc{
			domain.HarnessCodex:      sendCodexAppServer,
			domain.HarnessClaudeCode: sendClaudeStreamJSON,
		},
	}
}

func (m *providerMessenger) Send(ctx context.Context, id domain.SessionID, message string) error {
	_, err := m.SendWithDelivery(ctx, id, message)
	return err
}

func (m *providerMessenger) SendWithDelivery(ctx context.Context, id domain.SessionID, message string) (ports.MessageDelivery, error) {
	rec, ok, err := m.fallback.store.GetSession(ctx, id)
	if err != nil {
		return ports.MessageDeliveryTerminal, err
	}
	if !ok {
		return ports.MessageDeliveryTerminal, fmt.Errorf("session %s: %w", id, sessionmanager.ErrNotFound)
	}
	if rec.IsTerminated {
		return ports.MessageDeliveryTerminal, fmt.Errorf("session %s: %w", id, sessionmanager.ErrTerminated)
	}
	if rec.Activity.State == domain.ActivityExited {
		return ports.MessageDeliveryTerminal, fmt.Errorf("session %s: %w", id, sessionmanager.ErrTerminated)
	}
	if rec.Activity.State == domain.ActivityBlocked || rec.Metadata.PendingSubmitFingerprint != "" {
		return ports.MessageDeliveryTerminal, fmt.Errorf("session %s: %w", id, sessionmanager.ErrAwaitingDecision)
	}

	// Empty messages are Enter-only recovery nudges and are meaningful only to
	// the interactive terminal transport.
	nativeID := strings.TrimSpace(rec.Metadata.AgentSessionID)
	send, supported := m.providers[rec.Harness]
	if message == "" || nativeID == "" || !supported {
		return ports.MessageDeliveryTerminal, m.fallback.Send(ctx, id, message)
	}
	if m.restoreConfig == nil {
		m.logger.Warn("structured provider restore config unavailable; using terminal fallback",
			"sessionID", id, "harness", rec.Harness)
		return ports.MessageDeliveryTerminal, m.fallback.Send(ctx, id, message)
	}
	if m.environment == nil {
		m.logger.Warn("structured provider runtime environment unavailable; using terminal fallback",
			"sessionID", id, "harness", rec.Harness)
		return ports.MessageDeliveryTerminal, m.fallback.Send(ctx, id, message)
	}
	restore, restoreErr := m.restoreConfig(ctx, id)
	if restoreErr != nil {
		m.logger.Warn("structured provider restore config unavailable; using terminal fallback",
			"sessionID", id, "harness", rec.Harness, "error", restoreErr)
		return ports.MessageDeliveryTerminal, m.fallback.Send(ctx, id, message)
	}

	env, envErr := m.environment(ctx, id)
	if envErr != nil {
		m.logger.Warn("structured provider environment unavailable; using terminal fallback",
			"sessionID", id, "harness", rec.Harness, "error", envErr)
		return ports.MessageDeliveryTerminal, m.fallback.Send(ctx, id, message)
	}

	ackCtx, cancel := context.WithTimeout(ctx, providerAcknowledgementTimeout)
	defer cancel()
	delivery := providerDelivery{
		record:      rec,
		environment: env,
		restore:     restore,
		message:     message,
		finalCheck: func(checkCtx context.Context) error {
			return m.finalDeliveryCheck(checkCtx, rec)
		},
	}
	accepted, sendErr := send(ackCtx, delivery)
	if accepted {
		if sendErr != nil {
			m.logger.Warn("provider message accepted with a later transport error",
				"sessionID", id, "harness", rec.Harness, "error", sendErr)
		}
		return ports.MessageDeliveryStructured, nil
	}
	if sendErr != nil {
		if errors.Is(sendErr, errProviderSessionChanged) {
			return ports.MessageDeliveryTerminal, sendErr
		}
		m.logger.Warn("structured provider delivery unavailable; using terminal fallback",
			"sessionID", id, "harness", rec.Harness, "error", sendErr)
	}
	// The provider handshake may have consumed most of the acknowledgement
	// timeout. Revalidate the exact activity episode immediately before the raw
	// terminal write; the outer session guard's earlier read is no longer fresh
	// enough to safely paste + Enter into a possibly changed prompt.
	if checkErr := m.finalDeliveryCheck(ctx, rec); checkErr != nil {
		return ports.MessageDeliveryTerminal, checkErr
	}
	return ports.MessageDeliveryTerminal, m.fallback.Send(ctx, id, message)
}

func (m *providerMessenger) finalDeliveryCheck(ctx context.Context, expected domain.SessionRecord) error {
	current, ok, err := m.fallback.store.GetSession(ctx, expected.ID)
	if err != nil {
		return fmt.Errorf("%w: read session: %w", errProviderSessionChanged, err)
	}
	if !ok || current.IsTerminated || current.Activity.State == domain.ActivityExited {
		return fmt.Errorf("%w: session terminated", errProviderSessionChanged)
	}
	if current.Activity != expected.Activity ||
		current.Metadata.AgentSessionID != expected.Metadata.AgentSessionID ||
		current.Metadata.PendingSubmitFingerprint != expected.Metadata.PendingSubmitFingerprint {
		return fmt.Errorf("%w: activity episode no longer matches", errProviderSessionChanged)
	}
	if current.Activity.State == domain.ActivityBlocked || current.Metadata.PendingSubmitFingerprint != "" {
		return fmt.Errorf("%w: session awaits user input", errProviderSessionChanged)
	}
	return nil
}

type providerProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  <-chan []byte
	stderr *boundedBuffer
	done   <-chan error
}

func startProviderProcess(name string, args []string, dir string, env map[string]string) (*providerProcess, error) {
	cmd := exec.Command(name, args...) // #nosec G204 -- binaries are adapter-resolved; args are discrete
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), formatProviderEnv(env)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr := &boundedBuffer{limit: 16 << 10}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	lines := make(chan []byte, 32)
	done := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64<<10), 4<<20)
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			lines <- line
		}
		close(lines)
		if scanErr := scanner.Err(); scanErr != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			done <- scanErr
			close(done)
			return
		}
		done <- cmd.Wait()
		close(done)
	}()
	return &providerProcess{cmd: cmd, stdin: stdin, lines: lines, stderr: stderr, done: done}, nil
}

func formatProviderEnv(env map[string]string) []string {
	formatted := make([]string, 0, len(env))
	for key, value := range env {
		formatted = append(formatted, key+"="+value)
	}
	return formatted
}

func (p *providerProcess) stop() {
	_ = p.stdin.Close()
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}

func (p *providerProcess) waitLine(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		p.stop()
		return nil, ctx.Err()
	case line, ok := <-p.lines:
		if ok {
			return line, nil
		}
		err := <-p.done
		if err == nil {
			err = errors.New("provider process exited before acknowledgement")
		}
		if detail := strings.TrimSpace(p.stderr.String()); detail != "" {
			return nil, fmt.Errorf("%w: %s", err, detail)
		}
		return nil, err
	}
}

type boundedBuffer struct {
	mu    sync.Mutex
	limit int
	data  []byte
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if len(b.data) > b.limit {
		b.data = append([]byte(nil), b.data[len(b.data)-b.limit:]...)
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}

type rpcEnvelope struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func sendCodexAppServer(ctx context.Context, delivery providerDelivery) (bool, error) {
	rec := delivery.record
	threadID := delivery.restore.Session.Metadata[ports.MetadataKeyAgentSessionID]
	command, err := codex.AppServerCommand(ctx, delivery.restore)
	if err != nil {
		return false, err
	}
	proc, err := startProviderProcess(command[0], command[1:], rec.Metadata.WorkspacePath, delivery.environment)
	if err != nil {
		return false, fmt.Errorf("start codex app-server: %w", err)
	}
	accepted := false
	defer func() {
		if !accepted {
			proc.stop()
		}
	}()

	if err := writeRPC(proc.stdin, 1, "initialize", map[string]any{
		"clientInfo": map[string]string{"name": "agent-orchestrator", "version": "0.0.1"},
	}); err != nil {
		return false, err
	}
	if err := waitRPCResponse(ctx, proc, 1); err != nil {
		return false, fmt.Errorf("initialize codex app-server: %w", err)
	}
	if err := writeRPCNotification(proc.stdin, "initialized", map[string]any{}); err != nil {
		return false, err
	}
	if err := writeRPC(proc.stdin, 2, "thread/resume", map[string]any{
		"threadId": threadID,
		"cwd":      rec.Metadata.WorkspacePath,
	}); err != nil {
		return false, err
	}
	if err := waitRPCResponse(ctx, proc, 2); err != nil {
		return false, fmt.Errorf("resume codex thread: %w", err)
	}
	if err := delivery.finalCheck(ctx); err != nil {
		return false, err
	}
	if err := writeRPC(proc.stdin, 3, "turn/start", map[string]any{
		"threadId": threadID,
		"input": []map[string]string{{
			"type": "text",
			"text": delivery.message,
		}},
	}); err != nil {
		return false, err
	}
	if err := waitRPCResponse(ctx, proc, 3); err != nil {
		return false, fmt.Errorf("start codex turn: %w", err)
	}
	accepted = true

	// app-server owns the turn after its response. Keep the process alive until
	// completion, safely declining any hidden approval request rather than
	// guessing user intent. Sessions configured for no-prompt execution never
	// take this branch.
	go drainCodexTurn(proc)
	return true, nil
}

func writeRPC(w io.Writer, id int, method string, params any) error {
	return json.NewEncoder(w).Encode(map[string]any{
		"id": id, "method": method, "params": params,
	})
}

func writeRPCNotification(w io.Writer, method string, params any) error {
	return json.NewEncoder(w).Encode(map[string]any{
		"method": method, "params": params,
	})
}

func waitRPCResponse(ctx context.Context, proc *providerProcess, id int) error {
	wantID := fmt.Sprintf("%d", id)
	for {
		line, err := proc.waitLine(ctx)
		if err != nil {
			return err
		}
		var envelope rpcEnvelope
		if json.Unmarshal(line, &envelope) != nil || string(envelope.ID) != wantID {
			continue
		}
		if envelope.Error != nil {
			return fmt.Errorf("rpc error %d: %s", envelope.Error.Code, envelope.Error.Message)
		}
		if envelope.Result == nil {
			return errors.New("rpc response has no result")
		}
		return nil
	}
}

func drainCodexTurn(proc *providerProcess) {
	defer proc.stop()
	for line := range proc.lines {
		var envelope rpcEnvelope
		if json.Unmarshal(line, &envelope) != nil {
			continue
		}
		if envelope.Method == "turn/completed" {
			return
		}
		if len(envelope.ID) == 0 {
			continue
		}
		_ = writeCodexServerResponse(proc.stdin, envelope)
	}
}

func writeCodexServerResponse(w io.Writer, request rpcEnvelope) error {
	response := map[string]any{"id": request.ID}
	switch request.Method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		response["result"] = map[string]string{"decision": "decline"}
	case "item/tool/requestUserInput":
		response["result"] = map[string]any{"answers": map[string]any{}}
	case "mcpServer/elicitation/request":
		response["result"] = map[string]string{"action": "decline"}
	case "item/permissions/requestApproval":
		response["result"] = map[string]any{
			"permissions": map[string]any{},
			"scope":       "turn",
		}
	default:
		// Every server request must receive a response. Returning a JSON-RPC
		// method error makes unsupported client capabilities fail explicitly
		// instead of leaving an acknowledged turn waiting forever.
		response["error"] = map[string]any{
			"code":    -32601,
			"message": "unsupported by agent-orchestrator structured delivery",
		}
	}
	return json.NewEncoder(w).Encode(response)
}

func sendClaudeStreamJSON(ctx context.Context, delivery providerDelivery) (bool, error) {
	rec := delivery.record
	command, err := claudecode.StreamJSONCommand(ctx, delivery.restore)
	if err != nil {
		return false, err
	}
	proc, err := startProviderProcess(command[0], command[1:], rec.Metadata.WorkspacePath, delivery.environment)
	if err != nil {
		return false, fmt.Errorf("start claude stream-json: %w", err)
	}
	accepted := false
	defer func() {
		if !accepted {
			proc.stop()
		}
	}()

	input := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": []map[string]string{{"type": "text", "text": delivery.message}},
		},
	}
	if err := delivery.finalCheck(ctx); err != nil {
		return false, err
	}
	if err := json.NewEncoder(proc.stdin).Encode(input); err != nil {
		return false, err
	}
	// EOF tells print mode to finish after this turn. replay-user-messages emits
	// an explicit acknowledgement before the response is generated.
	if err := proc.stdin.Close(); err != nil {
		return false, err
	}

	for {
		line, err := proc.waitLine(ctx)
		if err != nil {
			return false, fmt.Errorf("await claude input acknowledgement: %w", err)
		}
		var event struct {
			Type    string `json:"type"`
			Message struct {
				Role string `json:"role"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &event) == nil && event.Type == "user" && event.Message.Role == "user" {
			accepted = true
			go func() {
				for line := range proc.lines {
					_ = line
				}
			}()
			return true, nil
		}
	}
}
