package hostclient

import (
	"context"
	"errors"
	"testing"

	"github.com/zachlatta/intelligence/internal/paseo"
)

func TestPermissionRecovery(t *testing.T) {
	for _, tc := range []struct {
		provider, mode string
		want           bool
	}{
		{"claude", "bypassPermissions", true}, {"codex", "full-access", true}, {"muse", "allowAll", true}, {"antigravity", "full-access", true},
		{"claude", "default", false}, {"claude", "", false}, {"unknown", "full-access", false}, {"codex", "auto", false},
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
	offline, refuse := true, true
	denies := 0
	var statuses []string
	s := PermissionSync{Send: func(_ context.Context, r PermissionResolution) error {
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
	if s.Sync(context.Background(), []paseo.Agent{a}) == nil {
		t.Fatal("lost denial error")
	}
	refuse = false
	if err := s.Sync(context.Background(), []paseo.Agent{a}); err != nil {
		t.Fatal(err)
	}
	if denies != 2 || statuses[len(statuses)-1] != "denied" {
		t.Fatal(denies, statuses)
	}
}

func TestPermissionAuditRetriesAfterPromptDisappears(t *testing.T) {
	a := paseo.Agent{ID: "a", Provider: "claude", CurrentModeID: "bypassPermissions", PendingPermissions: []paseo.Permission{{ID: "p"}}}
	denies, finals := 0, 0
	failFinal := true
	s := PermissionSync{Send: func(_ context.Context, r PermissionResolution) error {
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
	if err := s.Sync(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if denies != 1 || finals != 2 {
		t.Fatal(denies, finals)
	}
}
