package hostclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/paseo"
	"github.com/zachlatta/intelligence/internal/paseo/paseotest"
)

func TestWatcherTagsEachWorkspaceOnce(t *testing.T) {
	d := daemon(t)
	var mu sync.Mutex
	asked := map[string]int{}
	w := &Watcher{
		Dial: func(ctx context.Context) (*paseo.Client, error) {
			return paseo.Dial(ctx, paseo.Target{URL: d.URL(), Token: tok, ClientID: "test"})
		},
		RetryEvery: 50 * time.Millisecond,
		Tags: func(id string) ([]string, error) {
			mu.Lock()
			defer mu.Unlock()
			asked[id]++
			if id == "w1" {
				return []string{TagPaseoGUI}, nil
			}
			return nil, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go w.Run(ctx)
	s := eventually(t, w, "workspaces", func(s PaseoStatus) bool { return len(s.Workspaces) == 2 })
	if !slices.Equal(s.Workspaces[0].Tags, []string{"Paseo GUI"}) || s.Workspaces[1].Tags != nil {
		t.Errorf("tags = %v, %v", s.Workspaces[0].Tags, s.Workspaces[1].Tags)
	}
	// a workspace pushed later is tagged too; ones already tagged are not looked up again
	d.Push("workspace_update", map[string]any{"kind": "upsert", "workspace": paseotest.Workspace("w1", "worktree", "done")})
	d.Push("workspace_update", map[string]any{"kind": "upsert", "workspace": paseotest.Workspace("w9", "directory", "done")})
	eventually(t, w, "w9", func(s PaseoStatus) bool { return len(s.Workspaces) == 3 })
	mu.Lock()
	defer mu.Unlock()
	if asked["w1"] != 1 || asked["w2"] != 1 || asked["w9"] != 1 {
		t.Errorf("looked up %v; want each workspace once", asked)
	}
}

func TestAFailedTagLookupIsTriedAgain(t *testing.T) {
	d := daemon(t)
	var mu sync.Mutex
	fail := true
	w := &Watcher{
		Dial: func(ctx context.Context) (*paseo.Client, error) {
			return paseo.Dial(ctx, paseo.Target{URL: d.URL(), Token: tok, ClientID: "test"})
		},
		RetryEvery: 50 * time.Millisecond,
		Tags: func(id string) ([]string, error) {
			mu.Lock()
			defer mu.Unlock()
			if fail {
				return nil, os.ErrPermission
			}
			return []string{TagPaseoGUI}, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go w.Run(ctx)
	eventually(t, w, "workspaces", func(s PaseoStatus) bool { return len(s.Workspaces) == 2 && s.Workspaces[0].Tags == nil })
	mu.Lock()
	fail = false
	mu.Unlock()
	d.Push("workspace_update", map[string]any{"kind": "upsert", "workspace": paseotest.Workspace("w1", "worktree", "done")})
	eventually(t, w, "w1 tagged", func(s PaseoStatus) bool { return len(s.Workspaces[0].Tags) == 1 })
}

func TestPaseoHomeTagsWorkspacesMadeInTheApp(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "creations")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i, c := range []struct{ id, key string }{
		{"wks_aaaaaaaaaaaaaaaa", "draft_msg_1794182094800_sw78to36g"},
		{"wks_bbbbbbbbbbbbbbbb", "0c8e2b1e-5a6f-4c55-9d7e-0d6a3f2b9c11"},
	} {
		identity := strings.Repeat(string(rune('1'+i)), 64)
		sum := sha256.Sum256([]byte(`["workspace","` + c.id + `"]`))
		os.WriteFile(filepath.Join(dir, hex.EncodeToString(sum[:])+".claim"), []byte(identity), 0o600)
		os.WriteFile(filepath.Join(dir, identity+".json"), []byte(`{"snapshot":{"idempotencyKey":"`+c.key+`"}}`), 0o600)
	}
	tags := PaseoHomeTags(home)
	for id, want := range map[string][]string{
		"wks_aaaaaaaaaaaaaaaa": {"Paseo GUI"},
		"wks_bbbbbbbbbbbbbbbb": nil,
		"wks_cccccccccccccccc": nil, // no receipt
	} {
		if got, err := tags(id); err != nil || !slices.Equal(got, want) {
			t.Errorf("tags(%s) = %v, %v; want %v", id, got, err, want)
		}
	}
}
