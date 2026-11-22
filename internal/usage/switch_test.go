package usage

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// SwitchClaude finds the login's cswap slot as cswap lists it now (slots can change), switches
// with a backup of the outgoing login (no --force), and checks cswap then reports it active.
func TestSwitchClaudeUsesTheSlotCswapListsNow(t *testing.T) {
	active := "ada"
	var calls []string
	r := &Reader{Now: func() time.Time { return now }, Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if name != "cswap" {
			return nil, ErrMissing
		}
		switch strings.Join(args, " ") {
		case "list --json":
			if active == "grace" {
				return []byte(strings.NewReplacer(`"active":true`, `"active":false`, `"number":1,"email":"grace@example.com","active":false`, `"number":1,"email":"grace@example.com","active":true`).Replace(cswapJSON)), nil
			}
			return []byte(cswapJSON), nil
		case "switch 1 --json":
			active = "grace"
			return []byte(`{"switched":true}`), nil
		}
		return nil, errors.New("unexpected")
	}}
	slot, err := r.SwitchClaude(t.Context(), "grace@example.com")
	if err != nil || slot != 1 {
		t.Fatalf("switch: %d %v (calls %q)", slot, err, calls)
	}
	if strings.Join(calls, "; ") != "cswap list --json; cswap switch 1 --json; cswap list --json" {
		t.Fatalf("calls = %q", calls)
	}

	// Already active: nothing to change.
	calls = nil
	if slot, err := r.SwitchClaude(t.Context(), "grace@example.com"); err != nil || slot != 1 || len(calls) != 1 {
		t.Fatalf("repeat: %d %v %q", slot, err, calls)
	}
	// A login cswap does not hold, or one whose read failed, is refused before any switch.
	for _, login := range []string{"nobody@example.com", "linus@example.com"} {
		calls = nil
		if _, err := r.SwitchClaude(t.Context(), login); err == nil || len(calls) != 1 {
			t.Fatalf("%s: %v %q", login, err, calls)
		}
	}
}

// A switch cswap accepts but does not apply is a failure.
func TestSwitchClaudeChecksTheResult(t *testing.T) {
	r := &Reader{Now: func() time.Time { return now }, Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		if args[0] == "list" {
			return []byte(cswapJSON), nil
		}
		return []byte(`{}`), nil
	}}
	if _, err := r.SwitchClaude(t.Context(), "grace@example.com"); err == nil || !strings.Contains(err.Error(), "still") {
		t.Fatalf("want unconfirmed switch error: %v", err)
	}
}
