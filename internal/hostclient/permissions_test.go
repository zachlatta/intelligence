package hostclient

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/paseo"
)

func TestPermissionRecovery(t *testing.T) {
	for _, tc := range []struct {
		provider, mode string
		want           bool
	}{
		{"claude", "bypassPermissions", true}, {"codex", "full-access", true}, {"muse", "allowAll", true}, {"antigravity", "full-access", true},
		{"claude", "default", false}, {"claude", "", false}, {"unknown", "full-access", false}, {"unknown", "", false}, {"codex", "auto", false},
	} {
		t.Run(tc.provider+tc.mode, func(t *testing.T) {
			a := paseo.Agent{ID: "a", Provider: tc.provider, CurrentModeID: tc.mode, PendingPermissions: []paseo.Permission{{ID: "p", Name: "Bash"}}}
			var events []PermissionResolution
			denies := 0
			s := PermissionSync{Send: func(_ context.Context, r PermissionResolution) error { events = append(events, r); return nil },
				Deny: func(_ context.Context, agent, id string) error {
					denies++
					if agent != "a" || id != "p" {
						t.Fatal(agent, id)
					}
					return nil
				}}
			for range 2 {
				if err := s.Sync(context.Background(), []paseo.Agent{a}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.want {
				if denies != 1 || len(events) != 2 || events[0].Status != "pending" || events[1].Status != "denied" {
					t.Fatal(denies, events)
				}
			} else if denies != 0 || len(events) != 0 {
				t.Fatal(denies, events)
			}
		})
	}
}

func TestPermissionRecoveryRetriesAndAuditsBeforeDeny(t *testing.T) {
	a := paseo.Agent{ID: "a", Provider: "claude", CurrentModeID: "bypassPermissions", PendingPermissions: []paseo.Permission{{ID: "p"}}}
	now := time.Unix(1, 0)
	offline, refuse := true, true
	denies := 0
	var statuses []string
	s := PermissionSync{Now: func() time.Time { return now }, Send: func(_ context.Context, r PermissionResolution) error {
		if offline {
			return errors.New("offline")
		}
		statuses = append(statuses, r.Status)
		return nil
	},
		Deny: func(context.Context, string, string) error {
			denies++
			if refuse {
				return errors.New("refused")
			}
			return nil
		}}
	_ = s.Sync(context.Background(), []paseo.Agent{a})
	if denies != 0 {
		t.Fatal("denied without durable audit")
	}
	offline = false
	now = now.Add(time.Second)
	if s.Sync(context.Background(), []paseo.Agent{a}) == nil {
		t.Fatal("lost denial error")
	}
	refuse = false
	now = now.Add(time.Second)
	if err := s.Sync(context.Background(), []paseo.Agent{a}); err != nil {
		t.Fatal(err)
	}
	if denies != 2 || statuses[len(statuses)-1] != "denied" {
		t.Fatal(denies, statuses)
	}
}

func TestPermissionAuditRetriesAfterPromptDisappears(t *testing.T) {
	a := paseo.Agent{ID: "a", Provider: "claude", CurrentModeID: "bypassPermissions", PendingPermissions: []paseo.Permission{{ID: "p"}}}
	now := time.Unix(1, 0)
	denies, finals := 0, 0
	failFinal := true
	s := PermissionSync{Now: func() time.Time { return now }, Send: func(_ context.Context, r PermissionResolution) error {
		if r.Status == "denied" {
			finals++
			if failFinal {
				return errors.New("lost ack")
			}
		}
		return nil
	}, Deny: func(context.Context, string, string) error { denies++; return nil }}
	_ = s.Sync(context.Background(), []paseo.Agent{a})
	failFinal = false
	now = now.Add(time.Second)
	if err := s.Sync(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if denies != 1 || finals != 2 {
		t.Fatal(denies, finals)
	}
}

func TestPermissionRetryBackoffIsPerPromptAndCapped(t *testing.T) {
	now := time.Unix(1, 0)
	a := paseo.Agent{ID: "a", Provider: "claude", CurrentModeID: "bypassPermissions", PendingPermissions: []paseo.Permission{{ID: "p"}}}
	calls, writes := 0, 0
	s := PermissionSync{Now: func() time.Time { return now }, Send: func(context.Context, PermissionResolution) error { writes++; return nil }, Deny: func(context.Context, string, string) error { calls++; return errors.New("reject") }}
	_ = s.Sync(context.Background(), []paseo.Agent{a})
	delay := time.Second
	for i := 0; i < 11; i++ {
		before, w := calls, writes
		now = now.Add(delay - time.Nanosecond)
		_ = s.Sync(context.Background(), []paseo.Agent{a})
		if calls != before || writes != w {
			t.Fatal("retry before due", delay, calls, writes)
		}
		now = now.Add(time.Nanosecond)
		_ = s.Sync(context.Background(), []paseo.Agent{a})
		if calls != before+1 || writes != w+1 {
			t.Fatal("missing retry or rewrote intent", delay, calls, writes)
		}
		delay = min(delay*2, 5*time.Minute)
	}
	// Another prompt does not inherit p's backoff.
	a.PendingPermissions = append(a.PendingPermissions, paseo.Permission{ID: "p2"})
	before := calls
	_ = s.Sync(context.Background(), []paseo.Agent{a})
	if calls != before+1 {
		t.Fatal("backoff leaked across prompts")
	}
}

func TestPermissionStuckAndCleared(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(fmt.Sprint(reject), func(t *testing.T) {
			now := time.Unix(1, 0)
			var last PermissionResolution
			calls := 0
			a := paseo.Agent{ID: "a", Provider: "claude", CurrentModeID: "bypassPermissions", PendingPermissions: []paseo.Permission{{ID: "p"}}}
			s := PermissionSync{Now: func() time.Time { return now }, Send: func(_ context.Context, r PermissionResolution) error { last = r; return nil }, Deny: func(context.Context, string, string) error {
				calls++
				if reject {
					return errors.New("reject")
				}
				return nil
			}}
			_ = s.Sync(context.Background(), []paseo.Agent{a})
			now = now.Add(20 * time.Minute)
			_ = s.Sync(context.Background(), []paseo.Agent{a})
			if last.Status != "stuck" || last.StuckAt.IsZero() || last.FirstSeenAt.IsZero() {
				t.Fatal(last)
			}
			if !reject && calls != 1 {
				t.Fatal("repeated confirmed denial")
			}
			now = now.Add(time.Second)
			_ = s.Sync(context.Background(), nil)
			if last.Status != "cleared" || last.ClearedAt.IsZero() {
				t.Fatal(last)
			}
		})
	}
}

func TestPermissionRewriteIncludesLiteralAbsoluteRemovalRule(t *testing.T) {
	for _, part := range []string{"literal absolute paths", "no glob, variable, substitution or ~", "no cd earlier"} {
		if !strings.Contains(permissionRewrite, part) {
			t.Fatal("missing", part)
		}
	}
}

func TestPermissionRetryStopsWhenModeChanges(t *testing.T) {
	now := time.Unix(1, 0)
	calls := 0
	var last PermissionResolution
	a := paseo.Agent{ID: "a", Provider: "claude", CurrentModeID: "bypassPermissions", PendingPermissions: []paseo.Permission{{ID: "p"}}}
	s := PermissionSync{Now: func() time.Time { return now }, Send: func(_ context.Context, r PermissionResolution) error { last = r; return nil }, Deny: func(context.Context, string, string) error { calls++; return errors.New("reject") }}
	_ = s.Sync(context.Background(), []paseo.Agent{a})
	a.CurrentModeID = "default"
	now = now.Add(time.Hour)
	_ = s.Sync(context.Background(), []paseo.Agent{a})
	if calls != 1 || last.Status != "error" {
		t.Fatal("acted on a prompt outside never-ask mode", calls, last)
	}
}

func TestPermissionRestartClearsPersistedStuckPrompt(t *testing.T) {
	now := time.Unix(1, 0)
	var row PermissionResolution
	send := func(_ context.Context, r PermissionResolution) error { row = r; return nil }
	deny := func(context.Context, string, string) error { return nil }
	a := paseo.Agent{ID: "a", Provider: "claude", CurrentModeID: "bypassPermissions", PendingPermissions: []paseo.Permission{{ID: "p"}}}
	s := PermissionSync{Now: func() time.Time { return now }, Send: send, Deny: deny}
	_ = s.Sync(context.Background(), []paseo.Agent{a})
	now = now.Add(20 * time.Minute)
	_ = s.Sync(context.Background(), []paseo.Agent{a})
	if row.Status != "stuck" {
		t.Fatal(row)
	}
	s = PermissionSync{Now: func() time.Time { return now }, Send: send, Deny: deny,
		Load: func(context.Context) ([]PermissionResolution, error) { return []PermissionResolution{row}, nil }}
	now = now.Add(time.Second)
	if err := s.Sync(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if row.Status != "cleared" || row.ClearedAt.IsZero() {
		t.Fatal("restart left stale stuck state", row)
	}
}

func TestPermissionRestoreResumesBackoffAndStuckTracking(t *testing.T) {
	now := time.Unix(1, 0)
	row := PermissionResolution{AgentID: "a", PermissionID: "p", Provider: "claude", Mode: "bypassPermissions", Status: "error",
		FirstSeenAt: now.Add(-20 * time.Minute), At: now, NextRetryAt: now.Add(time.Minute), RetryDelay: time.Minute}
	calls, loads := 0, 0
	s := PermissionSync{Now: func() time.Time { return now },
		Load: func(context.Context) ([]PermissionResolution, error) {
			loads++
			return []PermissionResolution{row}, nil
		},
		Send: func(_ context.Context, r PermissionResolution) error { row = r; return nil },
		Deny: func(context.Context, string, string) error { calls++; return errors.New("reject") }}
	a := paseo.Agent{ID: "a", Provider: "claude", CurrentModeID: "bypassPermissions", PendingPermissions: []paseo.Permission{{ID: "p"}}}
	_ = s.Sync(context.Background(), []paseo.Agent{a})
	if row.Status != "stuck" || calls != 0 {
		t.Fatal("lost restored age or backoff", row, calls)
	}
	now = now.Add(time.Minute)
	_ = s.Sync(context.Background(), []paseo.Agent{a})
	if calls != 1 || row.RetryDelay != 2*time.Minute {
		t.Fatal(row, calls)
	}
	s.Reset()
	_ = s.Sync(context.Background(), []paseo.Agent{a})
	if loads != 2 || calls != 1 {
		t.Fatal("reconnect did not restore", loads, calls)
	}
}

func TestPermissionRestoreFailureBlocksRecoveryAndRetries(t *testing.T) {
	now := time.Unix(1, 0)
	calls, loads := 0, 0
	offline := true
	s := PermissionSync{Now: func() time.Time { return now }, Load: func(context.Context) ([]PermissionResolution, error) {
		loads++
		if offline {
			return nil, errors.New("offline")
		}
		return nil, nil
	},
		Send: func(context.Context, PermissionResolution) error { return nil }, Deny: func(context.Context, string, string) error { calls++; return nil }}
	a := paseo.Agent{ID: "a", Provider: "claude", CurrentModeID: "bypassPermissions", PendingPermissions: []paseo.Permission{{ID: "p"}}}
	_ = s.Sync(context.Background(), []paseo.Agent{a})
	_ = s.Sync(context.Background(), []paseo.Agent{a})
	if calls != 0 || loads != 1 {
		t.Fatal(calls, loads)
	}
	offline = false
	now = now.Add(time.Second)
	if err := s.Sync(context.Background(), []paseo.Agent{a}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal(calls)
	}
}

func TestPermissionRestoreDoesNotReplaceUnuploadedDenial(t *testing.T) {
	now := time.Unix(1, 0)
	old := PermissionResolution{AgentID: "a", PermissionID: "p", Provider: "claude", Mode: "bypassPermissions", Status: "pending", At: now, FirstSeenAt: now}
	fail := true
	calls := 0
	s := PermissionSync{Now: func() time.Time { return now }, Load: func(context.Context) ([]PermissionResolution, error) { return []PermissionResolution{old}, nil },
		Send: func(_ context.Context, r PermissionResolution) error {
			if fail && r.Status == "denied" {
				return errors.New("lost ack")
			}
			return nil
		},
		Deny: func(context.Context, string, string) error { calls++; return nil }}
	a := paseo.Agent{ID: "a", Provider: "claude", CurrentModeID: "bypassPermissions", PendingPermissions: []paseo.Permission{{ID: "p"}}}
	_ = s.Sync(context.Background(), []paseo.Agent{a})
	fail = false
	now = now.Add(time.Second)
	s.Reset()
	if err := s.Sync(context.Background(), []paseo.Agent{a}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("denied again after restoring stale row", calls)
	}
}

func TestPermissionRestoreKeepsConfirmedDenialAndOriginalAge(t *testing.T) {
	now := time.Unix(1, 0)
	row := PermissionResolution{AgentID: "a", PermissionID: "p", Provider: "claude", Mode: "bypassPermissions", Status: "denied",
		At: now, FirstSeenAt: now.Add(-time.Hour), DeniedAt: now.Add(-19 * time.Minute)}
	s := PermissionSync{Now: func() time.Time { return now }, Load: func(context.Context) ([]PermissionResolution, error) { return []PermissionResolution{row}, nil },
		Send: func(_ context.Context, r PermissionResolution) error { row = r; return nil },
		Deny: func(context.Context, string, string) error { t.Fatal("repeated restored confirmed denial"); return nil }}
	a := paseo.Agent{ID: "a", Provider: "claude", CurrentModeID: "bypassPermissions", PendingPermissions: []paseo.Permission{{ID: "p"}}}
	if err := s.Sync(context.Background(), []paseo.Agent{a}); err != nil {
		t.Fatal(err)
	}
	if row.Status != "denied" {
		t.Fatal("escalated before 20 minutes since denial", row)
	}
	now = now.Add(time.Minute)
	if err := s.Sync(context.Background(), []paseo.Agent{a}); err != nil {
		t.Fatal(err)
	}
	if row.Status != "stuck" {
		t.Fatal("lost denial age across restart", row)
	}
}
