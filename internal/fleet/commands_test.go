package fleet

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/devbox"
)

func commandRig(t *testing.T) *Registry {
	r := New(dbtest.New(t).Owner)
	record(t, r, full(), t1)
	return r
}

func enqueue(t *testing.T, r *Registry, kind, args string, at time.Time) Command {
	t.Helper()
	c, err := r.Enqueue(ctx, "devbox-1", kind, json.RawMessage(args), 10*time.Minute, at)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestEnqueueChecksTheBoxKindAndArgs(t *testing.T) {
	r := commandRig(t)
	c := enqueue(t, r, devbox.ArchiveAgentKind, `{"agentId":"a1"}`, t1)
	if c.ID == "" || c.Status != devbox.CommandQueued || !c.CreatedAt.Equal(t1) || !c.ExpiresAt.Equal(t1.Add(10*time.Minute)) {
		t.Errorf("command = %+v", c)
	}
	if _, err := r.Enqueue(ctx, "nobox", devbox.ArchiveAgentKind, json.RawMessage(`{"agentId":"a1"}`), time.Minute, t1); !errors.Is(err, ErrUnknownBox) {
		t.Errorf("unknown box: %v", err)
	}
	var bad *BadCommandError
	if _, err := r.Enqueue(ctx, "devbox-1", "explode", nil, time.Minute, t1); !errors.As(err, &bad) {
		t.Errorf("unknown kind: %v", err)
	}
	if _, err := r.Enqueue(ctx, "devbox-1", devbox.SendMessageKind, json.RawMessage(`{"agentId":"a1"}`), time.Minute, t1); !errors.As(err, &bad) {
		t.Errorf("message without text: %v", err)
	}
}

func TestClaimSendsEachQueuedCommandOnceInOrder(t *testing.T) {
	r := commandRig(t)
	a := enqueue(t, r, devbox.ArchiveAgentKind, `{"agentId":"a1"}`, t1)
	b := enqueue(t, r, devbox.CancelAgentKind, `{"agentId":"a2"}`, t1.Add(time.Second))
	got, err := r.Claim(ctx, "devbox-1", false, t1.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != a.ID || got[1].ID != b.ID || string(got[0].Args) != `{"agentId": "a1"}` {
		t.Fatalf("claimed %+v", got)
	}
	if again, _ := r.Claim(ctx, "devbox-1", false, t1.Add(3*time.Second)); len(again) != 0 {
		t.Errorf("claimed again: %+v", again)
	}
	c, _ := r.Command(ctx, a.ID)
	if c.Status != devbox.CommandSent || c.Attempts != 1 || !c.SentAt.Equal(t1.Add(2*time.Second)) {
		t.Errorf("after claim: %+v", c)
	}
}

func TestANewStreamResendsWhatHasNoResult(t *testing.T) {
	r := commandRig(t)
	a := enqueue(t, r, devbox.CreateAgentKind, `{"cwd":"/w","provider":"claude"}`, t1)
	r.Claim(ctx, "devbox-1", false, t1)
	if err := r.UpdateCommand(ctx, "devbox-1", devbox.CommandUpdate{ID: a.ID, Status: devbox.CommandRunning}, t1.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	// the stream dropped; the box dials again
	got, _ := r.Claim(ctx, "devbox-1", true, t1.Add(2*time.Second))
	if len(got) != 1 || got[0].ID != a.ID {
		t.Fatalf("resent %+v", got)
	}
	c, _ := r.Command(ctx, a.ID)
	if c.Status != devbox.CommandRunning || c.Attempts != 2 || !c.SentAt.Equal(t1) {
		t.Errorf("after resend: %+v (still running, first sent at t1)", c)
	}
}

func TestUpdateRecordsTheResult(t *testing.T) {
	r := commandRig(t)
	a := enqueue(t, r, devbox.CreateAgentKind, `{"cwd":"/w","provider":"claude"}`, t1)
	r.Claim(ctx, "devbox-1", false, t1)
	up := func(u devbox.CommandUpdate, at time.Time) error { return r.UpdateCommand(ctx, "devbox-1", u, at) }
	up(devbox.CommandUpdate{ID: a.ID, Status: devbox.CommandRunning}, t1.Add(time.Second))
	if err := up(devbox.CommandUpdate{ID: a.ID, Status: devbox.CommandSucceeded, Result: json.RawMessage(`{"agentId":"a9"}`)}, t1.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	c, _ := r.Command(ctx, a.ID)
	if c.Status != devbox.CommandSucceeded || string(c.Result) != `{"agentId": "a9"}` || !c.StartedAt.Equal(t1.Add(time.Second)) ||
		!c.FinishedAt.Equal(t1.Add(2*time.Second)) {
		t.Errorf("command = %+v", c)
	}
	// a late "running" (it was resent) does not undo the result
	up(devbox.CommandUpdate{ID: a.ID, Status: devbox.CommandRunning}, t1.Add(3*time.Second))
	if c, _ := r.Command(ctx, a.ID); c.Status != devbox.CommandSucceeded {
		t.Errorf("status = %s after a late running", c.Status)
	}
	if err := up(devbox.CommandUpdate{ID: a.ID, Status: devbox.CommandQueued}, t1); err == nil {
		t.Errorf("a box cannot set queued")
	}
	if err := r.UpdateCommand(ctx, "other", devbox.CommandUpdate{ID: a.ID, Status: devbox.CommandFailed}, t1); !errors.Is(err, ErrUnknownCommand) {
		t.Errorf("another box's command: %v", err)
	}
	if err := up(devbox.CommandUpdate{ID: "not-a-uuid", Status: devbox.CommandFailed}, t1); !errors.Is(err, ErrUnknownCommand) {
		t.Errorf("bad id: %v", err)
	}
}

func TestCommandsExpire(t *testing.T) {
	r := commandRig(t)
	never := enqueue(t, r, devbox.ArchiveAgentKind, `{"agentId":"a1"}`, t1)
	lost := enqueue(t, r, devbox.ArchiveAgentKind, `{"agentId":"a2"}`, t1)
	r.Claim(ctx, "devbox-1", false, t1) // both sent
	r.UpdateCommand(ctx, "devbox-1", devbox.CommandUpdate{ID: never.ID, Status: devbox.CommandFailed, Error: "x"}, t1)
	queued := enqueue(t, r, devbox.ArchiveAgentKind, `{"agentId":"a3"}`, t1)
	if got, _ := r.Claim(ctx, "devbox-1", true, t1.Add(11*time.Minute)); len(got) != 0 {
		t.Fatalf("claimed expired commands: %+v", got)
	}
	for id, want := range map[string]string{never.ID: devbox.CommandFailed, lost.ID: devbox.CommandExpired, queued.ID: devbox.CommandExpired} {
		c, _ := r.Command(ctx, id)
		if c.Status != want || (want == devbox.CommandExpired && (c.Error == "" || c.FinishedAt.IsZero())) {
			t.Errorf("%s: %+v, want %s", id, c, want)
		}
	}
	// a result that arrives late is still the truth
	r.UpdateCommand(ctx, "devbox-1", devbox.CommandUpdate{ID: lost.ID, Status: devbox.CommandSucceeded}, t1.Add(12*time.Minute))
	if c, _ := r.Command(ctx, lost.ID); c.Status != devbox.CommandSucceeded || c.Error != "" {
		t.Errorf("late result: %+v", c)
	}
}
