// client.go - loopback TCP client helpers that mirror pty-client.ts.
// Each function dials the host addr fresh (short-lived connection) and
// returns without maintaining state. Cross-platform: uses only stdlib net.
package conpty

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"syscall"
	"time"
)

const (
	// ptyInputChunkRunes is the max runes per terminal-input frame.
	// Mirrors PTY_INPUT_CHUNK_CHARS in pty-client.ts.
	ptyInputChunkRunes = 512
	// ptyInputChunkDelay is the inter-chunk delay. Mirrors PTY_INPUT_CHUNK_DELAY_MS.
	ptyInputChunkDelay = 15 * time.Millisecond
	// ptyInputEnterDelay is the pause before sending Enter. Mirrors PTY_INPUT_ENTER_DELAY_MS.
	ptyInputEnterDelay = 300 * time.Millisecond
	// ptyInputLongEnterDelay gives a multi-frame paste longer to settle in the
	// terminal editor before Enter is submitted. Without this, Codex can leave
	// the collapsed "[Pasted Content ...]" draft unsubmitted.
	ptyInputLongEnterDelay = time.Second

	dialTimeout      = 3 * time.Second
	getOutputTimeout = 3 * time.Second
	isAliveTimeout   = 2 * time.Second
)

type hostIdentityMismatchError struct {
	got  StatusPayload
	want StatusPayload
}

func (e *hostIdentityMismatchError) Error() string {
	return fmt.Sprintf("conpty: host identity mismatch: got session=%q generation=%q hostPid=%d; want session=%q generation=%q hostPid=%d", e.got.SessionID, e.got.Generation, e.got.HostPID, e.want.SessionID, e.want.Generation, e.want.HostPID)
}

// dialHost opens a TCP connection to addr with a deadline. Callers close it.
var dialHost = func(addr string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("tcp", addr, timeout)
}

// clientSendMessage chunks message by 512 runes and sends each as a
// MsgTerminalInput frame with 15ms gaps, then pauses long enough for the paste
// to settle (1s for payloads over 512 runes, 300ms otherwise) and sends "\r".
// Mirrors ptyHostSendMessage from pty-client.ts.
func clientSendMessage(addr, sessionID, generation string, hostPID int, message string) error {
	conn, _, _, err := dialVerifiedHost(addr, sessionID, generation, hostPID)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	runes := []rune(message)
	for i := 0; i < len(runes); i += ptyInputChunkRunes {
		end := i + ptyInputChunkRunes
		if end > len(runes) {
			end = len(runes)
		}
		chunk := string(runes[i:end])
		frame, err := EncodeMessage(MsgTerminalInput, []byte(chunk))
		if err != nil {
			return err
		}
		if _, err := conn.Write(frame); err != nil {
			return err
		}
		// Inter-chunk delay only between chunks, not after the last one.
		if end < len(runes) {
			time.Sleep(ptyInputChunkDelay)
		}
	}

	// Brief pause before Enter (matches TS: Enter sent as a separate frame).
	// Skipped for an empty message — an Enter-only nudge has no paste to let
	// settle, and the pause would only widen the guard-read→Enter window
	// (mirrors the tmux runtime's enterDelay contract).
	if len(runes) > 0 {
		time.Sleep(inputEnterDelay(len(runes)))
	}
	frame, err := EncodeMessage(MsgTerminalInput, []byte("\r"))
	if err != nil {
		return err
	}
	_, err = conn.Write(frame)
	return err
}

func inputEnterDelay(runes int) time.Duration {
	if runes > ptyInputChunkRunes {
		return ptyInputLongEnterDelay
	}
	return ptyInputEnterDelay
}

func clientSendInput(addr, sessionID, generation string, hostPID int, input string) error {
	conn, _, _, err := dialVerifiedHost(addr, sessionID, generation, hostPID)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	frame, err := EncodeMessage(MsgTerminalInput, []byte(input))
	if err != nil {
		return err
	}
	_, err = conn.Write(frame)
	return err
}

// clientGetOutput sends MsgGetOutputReq and reads frames until MsgGetOutputRes.
// Returns "" on timeout or connection failure (no error), matching the TS.
// lines <= 0 is handled by the caller (runtime.go rejects it before calling).
func clientGetOutput(addr, sessionID, generation string, hostPID, lines int) (string, error) {
	conn, _, residual, err := dialVerifiedHost(addr, sessionID, generation, hostPID)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetDeadline(time.Now().Add(getOutputTimeout))

	req, _ := json.Marshal(GetOutputReq{Lines: lines})
	reqFrame, _ := EncodeMessage(MsgGetOutputReq, req) // req is small JSON, never overflows uint32
	if _, err := conn.Write(reqFrame); err != nil {
		return "", nil
	}

	resultC := make(chan string, 1)
	parser := NewMessageParser(func(msgType byte, payload []byte) {
		if msgType == MsgGetOutputRes {
			select {
			case resultC <- string(payload):
			default:
			}
		}
	})
	parser.Feed(residual)

	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			parser.Feed(buf[:n])
		}
		select {
		case text := <-resultC:
			return text, nil
		default:
		}
		if err != nil {
			break
		}
	}
	// Drain the channel one last time after the read loop ends.
	select {
	case text := <-resultC:
		return text, nil
	default:
		return "", nil // timeout or EOF before response
	}
}

// clientIsAlive probes the host with MsgStatusReq and distinguishes three
// outcomes for the reaper (see IsAlive in runtime.go):
//
//   - alive==true,  transientErr==nil: a valid MsgStatusRes was received.
//   - alive==false, transientErr==nil: the host is DEFINITIVELY gone (the dial
//     was refused: nothing is listening on the loopback addr).
//   - alive==false, transientErr!=nil: a TRANSIENT probe failure (network
//     timeout, or any connected-then-failed I/O error). The reaper records this
//     as ProbeFailed and retries instead of reaping a possibly-live session.
//
// When unsure, we prefer transient (return the error) rather than reporting
// death. Mirrors ptyHostIsAlive from pty-client.ts on the alive path: host
// reachable == alive, regardless of the inner agent's alive field.
func clientIsAlive(addr, sessionID, generation string, hostPID int) (alive bool, transientErr error) {
	conn, err := dialHost(addr, isAliveTimeout)
	if err != nil {
		// A dial timeout is transient (the loopback hiccupped). A refused
		// connection means nothing is listening -> definitively gone. Any
		// other dial failure is treated as transient ("when unsure, retry").
		if isTimeout(err) {
			return false, err
		}
		if isConnRefused(err) {
			return false, nil
		}
		return false, err
	}
	defer func() { _ = conn.Close() }()
	_, _, err = verifyHostConn(conn, sessionID, generation, hostPID)
	return err == nil, err
}

// dialVerifiedHost binds every operation to the identity returned on the same
// TCP connection. A loopback port reused after registry resolution cannot
// receive input, output, attach, or kill traffic for another generation.
func dialVerifiedHost(addr, sessionID, generation string, hostPID int) (net.Conn, [][]byte, []byte, error) {
	conn, err := dialHost(addr, dialTimeout)
	if err != nil {
		return nil, nil, nil, err
	}
	prefetched, residual, err := verifyHostConn(conn, sessionID, generation, hostPID)
	if err != nil {
		_ = conn.Close()
		return nil, nil, nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, prefetched, residual, nil
}

func verifyHostConn(conn net.Conn, sessionID, generation string, hostPID int) ([][]byte, []byte, error) {
	_ = conn.SetDeadline(time.Now().Add(isAliveTimeout))
	statusReqFrame, _ := EncodeMessage(MsgStatusReq, nil)
	if _, err := conn.Write(statusReqFrame); err != nil {
		return nil, nil, err
	}
	var prefetched [][]byte
	var status *StatusPayload
	var statusErr error
	parser := NewMessageParser(func(msgType byte, payload []byte) {
		switch msgType {
		case MsgTerminalData:
			prefetched = append(prefetched, append([]byte(nil), payload...))
		case MsgStatusRes:
			var sp StatusPayload
			if err := json.Unmarshal(payload, &sp); err != nil {
				statusErr = fmt.Errorf("conpty: malformed host identity: %w", err)
				return
			}
			if sp.SessionID == "" || sp.Generation == "" || sp.HostPID == 0 {
				statusErr = fmt.Errorf("conpty: missing host identity: session=%q generation=%q hostPid=%d", sp.SessionID, sp.Generation, sp.HostPID)
				return
			}
			if sp.SessionID != sessionID || sp.Generation != generation || sp.HostPID != hostPID {
				statusErr = &hostIdentityMismatchError{got: sp, want: StatusPayload{SessionID: sessionID, Generation: generation, HostPID: hostPID}}
				return
			}
			status = &sp
		}
	})
	buf := make([]byte, 4096)
	for status == nil && statusErr == nil {
		n, err := conn.Read(buf)
		if n > 0 {
			parser.Feed(buf[:n])
		}
		if err != nil && status == nil && statusErr == nil {
			return nil, nil, err
		}
	}
	if statusErr != nil {
		return nil, nil, statusErr
	}
	return prefetched, append([]byte(nil), parser.buf...), nil
}

// isTimeout reports whether err is a network timeout (dial timeout or
// read-deadline expiry). Cross-platform via the net.Error interface.
func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// isConnRefused reports whether err is a fast "connection refused" dial
// failure (nothing listening). errors.Is(ECONNREFUSED) covers Unix and modern
// Windows; the explicit WSAECONNREFUSED (10061) guards older Windows runtimes
// where the errno is not mapped to syscall.ECONNREFUSED.
func isConnRefused(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	const wsaeconnrefused = syscall.Errno(10061)
	return errors.Is(err, wsaeconnrefused)
}

// clientKill verifies identity and sends MsgKillReq on the same connection.
func clientKill(addr, sessionID, generation string, hostPID int) error {
	conn, _, _, err := dialVerifiedHost(addr, sessionID, generation, hostPID)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(isAliveTimeout))
	payload, err := json.Marshal(KillPayload{SessionID: sessionID, Generation: generation})
	if err != nil {
		return err
	}
	killFrame, err := EncodeMessage(MsgKillReq, payload)
	if err != nil {
		return err
	}
	_, err = conn.Write(killFrame)
	return err
}
