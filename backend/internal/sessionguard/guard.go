// Package sessionguard owns the one invariant every write into a live
// session's pane must satisfy: re-read the session immediately before writing
// and refuse when the paste could land somewhere only the user may act. The
// runtime appends Enter after every paste, so a write into a session paused on
// a permission/approval dialog would answer the decision on the user's behalf
// — an unrecoverable action, unlike a skipped message which callers re-attempt
// or surface. Every pane-writing path (user sends, post-send Enter nudges,
// lifecycle reaction nudges) funnels through this guard so the stale-state
// check lives in one tested place instead of being re-derived per call-site.
package sessionguard

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// SessionReader is the single store read the guard needs: the session's
// current liveness and activity state.
type SessionReader interface {
	GetSession(ctx context.Context, id domain.SessionID) (domain.SessionRecord, bool, error)
}

// Outcome reports what a guarded write did. Anything other than Sent means the
// message did NOT reach the pane; callers that record delivery must not stamp
// a suppressed write as delivered.
type Outcome int

const (
	// SuppressedUnknown is returned when the pre-write session read failed, so
	// the state is unknown and the guard failed closed. Deliberately the zero
	// value — a forgotten assignment must never read as a successful send.
	SuppressedUnknown Outcome = iota
	// Sent means the message was written to the session's pane (a messenger
	// failure surfaces as Sent plus a non-nil error: the write was attempted).
	Sent
	// SuppressedNotFound means no session row exists for the id.
	SuppressedNotFound
	// SuppressedTerminated means the session is terminated; its pane is gone
	// or about to be reaped.
	SuppressedTerminated
	// SuppressedAwaitingUser means the session awaits the human — blocked on a
	// permission decision (Deliver and Nudge), or waiting at the prompt for
	// the next instruction (Nudge only).
	SuppressedAwaitingUser
	// SuppressedStaleEpisode means a call-specific activity episode predicate
	// no longer matched at the guard's final pre-write store read.
	SuppressedStaleEpisode
	// SuppressedRateLimited means the provider parked the live harness on a
	// usage-limit failure. Automated input is withheld; an explicit Deliver is
	// still the intentional retry path after the provider window resets.
	SuppressedRateLimited
)

// String names the outcome for logs.
func (o Outcome) String() string {
	switch o {
	case Sent:
		return "sent"
	case SuppressedNotFound:
		return "suppressed_not_found"
	case SuppressedTerminated:
		return "suppressed_terminated"
	case SuppressedAwaitingUser:
		return "suppressed_awaiting_user"
	case SuppressedStaleEpisode:
		return "suppressed_stale_episode"
	case SuppressedRateLimited:
		return "suppressed_rate_limited"
	default:
		return "suppressed_unknown"
	}
}

// Guard is the guarded pane-write primitive shared by the session manager and
// lifecycle. It takes no locks of its own, so callers may hold theirs across a
// call (lifecycle's sendOnce calls it under react.mu). It implements
// ports.AgentMessenger (via Send) so it can transparently replace a raw
// messenger wherever only the error matters.
type Guard struct {
	store     SessionReader
	messenger ports.AgentMessenger
	logger    *slog.Logger
}

var _ ports.AgentMessenger = (*Guard)(nil)

// New builds a Guard over the store it re-reads and the messenger it writes
// through. A nil logger falls back to slog.Default().
func New(store SessionReader, messenger ports.AgentMessenger, logger *slog.Logger) *Guard {
	if logger == nil {
		logger = slog.Default()
	}
	return &Guard{store: store, messenger: messenger, logger: logger}
}

// Send satisfies ports.AgentMessenger so a Guard can sit in for the raw
// messenger. It applies the Deliver policy but FOLDS a suppressed outcome into
// nil: a caller that learns only "did Send error?" cannot tell that the write
// was actually refused. That is fine for callers that only need a best-effort
// delivery, but paths whose success CONTRACT depends on the write landing
// (after-start prompt delivery in Spawn/Restore) must call Deliver directly and
// map non-Sent outcomes to an error, or a session that terminates or blocks
// before injection is reported as a successful spawn with a prompt that was
// never delivered.
func (g *Guard) Send(ctx context.Context, id domain.SessionID, msg string) error {
	_, err := g.Deliver(ctx, id, msg)
	return err
}

// Deliver writes a user-initiated message or an Enter-only re-submit. A
// non-empty explicit message remains the intentional retry path after a usage
// limit resets. An empty message is AO automation, so a final-read rate-limit
// transition suppresses it alongside blocked decisions. waiting_input does not
// suppress: that idle prompt is exactly where an Enter re-submit belongs.
func (g *Guard) Deliver(ctx context.Context, id domain.SessionID, msg string) (Outcome, error) {
	return g.send(ctx, id, msg, func(rec domain.SessionRecord) Outcome {
		switch rec.Activity.State {
		case domain.ActivityBlocked:
			return SuppressedAwaitingUser
		case domain.ActivityRateLimited:
			if msg == "" {
				return SuppressedRateLimited
			}
			return Sent
		default:
			return Sent
		}
	}, nil)
}

// Nudge writes an AO-initiated (unsolicited) message into the session. It
// refuses whenever the session awaits the human in any form — blocked on a
// decision or waiting at the prompt — and while a prior prompt is durably
// latched as pending in the editor. An automated paste+Enter in those states
// could answer a dialog or stack text behind a draft that has not submitted.
func (g *Guard) Nudge(ctx context.Context, id domain.SessionID, msg string) (Outcome, error) {
	return g.send(ctx, id, msg, nudgeRefusal, nil)
}

// NudgeIdleEpisode writes an idle-review reminder only when the guard's final
// pre-write read still belongs to the exact idle episode that authorized it.
func (g *Guard) NudgeIdleEpisode(ctx context.Context, id domain.SessionID, msg string, idleSince time.Time) (Outcome, error) {
	return g.send(ctx, id, msg, nudgeRefusal, func(rec domain.SessionRecord) bool {
		return rec.Activity.State == domain.ActivityIdle && rec.Activity.LastActivityAt.Equal(idleSince)
	})
}

func nudgeRefusal(rec domain.SessionRecord) Outcome {
	if rec.Activity.State == domain.ActivityRateLimited {
		return SuppressedRateLimited
	}
	if rec.Activity.State.NeedsInput() || rec.Metadata.PendingSubmitFingerprint != "" {
		return SuppressedAwaitingUser
	}
	return Sent
}

// send re-reads the session immediately before pasting so the window between
// "state looked safe" and "bytes hit the pane" is as small as this process can
// make it. It is not atomic against the agent itself — a dialog can still
// appear mid-paste — but the just-in-time read is the strongest guarantee
// available without scraping the terminal. Fail closed: a store error
// suppresses the write rather than pressing Enter on an unknown state.
func (g *Guard) send(ctx context.Context, id domain.SessionID, msg string, refuse func(domain.SessionRecord) Outcome, require func(domain.SessionRecord) bool) (Outcome, error) {
	rec, ok, err := g.store.GetSession(ctx, id)
	if err != nil {
		return SuppressedUnknown, fmt.Errorf("guard %s: read session: %w", id, err)
	}
	if !ok {
		g.logger.Info("sessionguard: write suppressed", "sessionID", id, "reason", "not_found")
		return SuppressedNotFound, nil
	}
	// ActivityExited is refused alongside IsTerminated as defense-in-depth:
	// every exited writer today also sets IsTerminated, but a pane whose agent
	// exited execs an interactive shell, so a paste+Enter there would run the
	// message as shell commands — the invariant must not depend on writer
	// discipline alone.
	if rec.IsTerminated || rec.Activity.State == domain.ActivityExited {
		g.logger.Info("sessionguard: write suppressed", "sessionID", id, "reason", "terminated")
		return SuppressedTerminated, nil
	}
	if outcome := refuse(rec); outcome != Sent {
		reason := "awaiting_user"
		if outcome == SuppressedRateLimited {
			reason = "rate_limited"
		} else if rec.Metadata.PendingSubmitFingerprint != "" && !rec.Activity.State.NeedsInput() {
			reason = "pending_submit"
		}
		g.logger.Info("sessionguard: write suppressed", "sessionID", id, "reason", reason, "state", string(rec.Activity.State))
		return outcome, nil
	}
	if require != nil && !require(rec) {
		g.logger.Info("sessionguard: write suppressed", "sessionID", id, "reason", "stale_episode", "state", string(rec.Activity.State))
		return SuppressedStaleEpisode, nil
	}
	if err := g.messenger.Send(ctx, id, msg); err != nil {
		return Sent, fmt.Errorf("guard %s: send: %w", id, err)
	}
	return Sent, nil
}
