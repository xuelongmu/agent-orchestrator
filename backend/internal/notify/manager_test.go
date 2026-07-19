package notify

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type fakeStore struct {
	rows      []domain.NotificationRecord
	duplicate bool
	err       error
}

func (f *fakeStore) CreateNotification(_ context.Context, rec domain.NotificationRecord) (domain.NotificationRecord, bool, error) {
	if f.err != nil {
		return domain.NotificationRecord{}, false, f.err
	}
	if f.duplicate {
		return domain.NotificationRecord{}, false, nil
	}
	f.rows = append(f.rows, rec)
	return rec, true, nil
}

func TestManagerNotifyPersistsThenPublishes(t *testing.T) {
	st := &fakeStore{}
	hub := NewHub()
	ch, unsub := hub.Subscribe("")
	defer unsub()
	now := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	mgr := New(Deps{Store: st, Publisher: hub, Clock: func() time.Time { return now }, NewID: func() string { return "ntf_1" }})

	if err := mgr.Notify(context.Background(), Intent{Type: domain.NotificationNeedsInput, SessionID: "mer-1", ProjectID: "mer", SessionDisplayName: "checkout-flow"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if len(st.rows) != 1 {
		t.Fatalf("stored rows = %d, want 1", len(st.rows))
	}
	if got := st.rows[0]; got.ID != "ntf_1" || got.CreatedAt != now || got.Status != domain.NotificationUnread || got.Title != "checkout-flow needs input" {
		t.Fatalf("stored notification = %+v", got)
	}
	select {
	case got := <-ch:
		if got.ID != "ntf_1" {
			t.Fatalf("published = %+v", got)
		}
	default:
		t.Fatal("expected published notification")
	}
}

func TestManagerNotifyDuplicateDoesNotPublish(t *testing.T) {
	st := &fakeStore{duplicate: true}
	hub := NewHub()
	ch, unsub := hub.Subscribe("")
	defer unsub()
	mgr := New(Deps{Store: st, Publisher: hub, Clock: func() time.Time { return time.Now() }, NewID: func() string { return "ntf_1" }})

	if err := mgr.Notify(context.Background(), Intent{Type: domain.NotificationNeedsInput, SessionID: "mer-1", ProjectID: "mer", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Notify duplicate: %v", err)
	}
	select {
	case got := <-ch:
		t.Fatalf("duplicate published %+v", got)
	default:
	}
}

func TestManagerNotifyUsesHumanHandoffCopy(t *testing.T) {
	st := &fakeStore{}
	mgr := New(Deps{Store: st, Clock: time.Now, NewID: func() string { return "ntf_1" }})
	intent := Intent{
		Type:          domain.NotificationNeedsInput,
		SessionID:     "mer-1",
		ProjectID:     "mer",
		PRURL:         "https://github.com/o/r/pull/1",
		TitleOverride: "PR feedback is waiting",
		BodyOverride:  "CI feedback could not be sent while editor input is pending.",
	}
	if err := mgr.Notify(context.Background(), intent); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if len(st.rows) != 1 || st.rows[0].Title != intent.TitleOverride || st.rows[0].Body != intent.BodyOverride {
		t.Fatalf("stored notification = %+v, want handoff copy", st.rows)
	}
}

func TestManagerNotifyPersistsDaemonScopedControlPlaneIntent(t *testing.T) {
	st := &fakeStore{}
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	mgr := New(Deps{Store: st, Clock: func() time.Time { return now }, NewID: func() string { return "ntf_control" }})

	if err := mgr.Notify(context.Background(), Intent{
		Type:          domain.NotificationControlPlaneFailed,
		TitleOverride: "GitHub authentication needs attention",
		BodyOverride:  "Run `gh auth login` and restart AO.",
	}); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	if len(st.rows) != 1 {
		t.Fatalf("stored rows = %d, want 1", len(st.rows))
	}
	got := st.rows[0]
	if got.ID != "ntf_control" || got.SessionID != "" || got.ProjectID != "" || got.Type != domain.NotificationControlPlaneFailed {
		t.Fatalf("stored control notification = %#v", got)
	}
}

func TestManagerNotifyRejectsUnknownType(t *testing.T) {
	mgr := New(Deps{Store: &fakeStore{}, Clock: func() time.Time { return time.Now() }})
	err := mgr.Notify(context.Background(), Intent{Type: "surprise", SessionID: "mer-1", ProjectID: "mer"})
	if !errors.Is(err, domain.ErrInvalidNotificationType) {
		t.Fatalf("err = %v, want invalid type", err)
	}
}

func TestHubProjectFilter(t *testing.T) {
	hub := NewHub()
	ch, unsub := hub.Subscribe("mer")
	defer unsub()
	_ = hub.Publish(context.Background(), domain.NotificationRecord{ID: "skip", ProjectID: "ao"})
	_ = hub.Publish(context.Background(), domain.NotificationRecord{ID: "keep", ProjectID: "mer"})
	select {
	case got := <-ch:
		if got.ID != "keep" {
			t.Fatalf("published = %+v", got)
		}
	default:
		t.Fatal("expected filtered notification")
	}
}

func TestHubBroadcastsControlPlaneNotificationsWithoutWeakeningProjectIsolation(t *testing.T) {
	hub := NewHub()
	merCh, unsubscribeMer := hub.Subscribe("mer")
	defer unsubscribeMer()
	aoCh, unsubscribeAO := hub.Subscribe("ao")
	defer unsubscribeAO()

	_ = hub.Publish(context.Background(), domain.NotificationRecord{ID: "mer-only", Type: domain.NotificationNeedsInput, ProjectID: "mer"})
	select {
	case got := <-merCh:
		if got.ID != "mer-only" {
			t.Fatalf("project notification = %+v", got)
		}
	default:
		t.Fatal("matching project subscriber did not receive ordinary notification")
	}
	select {
	case got := <-aoCh:
		t.Fatalf("other project subscriber received ordinary notification: %+v", got)
	default:
	}

	control := domain.NotificationRecord{ID: "control", Type: domain.NotificationControlPlaneFailed}
	_ = hub.Publish(context.Background(), control)
	for project, ch := range map[string]<-chan domain.NotificationRecord{"mer": merCh, "ao": aoCh} {
		select {
		case got := <-ch:
			if got.ID != control.ID {
				t.Fatalf("%s control notification = %+v", project, got)
			}
		default:
			t.Fatalf("%s project subscriber did not receive daemon control notification", project)
		}
	}
}
