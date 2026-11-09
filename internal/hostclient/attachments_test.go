package hostclient

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/attachment"
	"github.com/zachlatta/intelligence/internal/paseo/paseotest"
)

func TestImageDeliveryBeforeAgentStarts(t *testing.T) {
	scratch := filepath.Join("..", "..", ".factory-scratch")
	if err := os.MkdirAll(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(scratch, "images-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	var data bytes.Buffer
	png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	img := attachment.Image{ID: "00000000-0000-4000-8000-000000000001", Data: data.Bytes(), MediaType: "image/png"}
	d := daemon(t)
	w := watcher(t, d)
	eventually(t, w, "connected", func(s PaseoStatus) bool { return s.Connected })
	args := CreateAgentArgs{Cwd: dir, Provider: "claude", Prompt: "See " + img.Path(), Attachments: []attachment.Image{img}}
	raw, _ := json.Marshal(args)
	if _, err := execute(t, w, CreateAgentKind, string(raw)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, img.Path())
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, img.Data) {
		t.Fatalf("delivered image: %v", err)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("image permissions: %v %v", st, err)
	}
	// Re-delivery is safe; a repository's pre-existing file must never be overwritten.
	if err := materializeImages(dir, []attachment.Image{img}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, []byte("existing"), 0600)
	if err := materializeImages(dir, []attachment.Image{img}); err == nil {
		t.Fatal("overwrote existing file")
	}
	os.Remove(path)
	os.Symlink(filepath.Join(dir, "target"), path)
	if err := materializeImages(dir, []attachment.Image{img}); err == nil {
		t.Fatal("followed symlink")
	}
}

func TestImageDeliveryToExistingSession(t *testing.T) {
	for _, behavior := range []string{"steer", "interrupt", "queue"} {
		t.Run(behavior, func(t *testing.T) {
			scratch := filepath.Join("..", "..", ".factory-scratch")
			os.MkdirAll(scratch, 0700)
			dir, err := os.MkdirTemp(scratch, "chat-images-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(dir) })
			dir, err = filepath.Abs(dir)
			if err != nil {
				t.Fatal(err)
			}
			var data bytes.Buffer
			png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 2, 2)))
			img := attachment.Image{ID: "00000000-0000-4000-8000-000000000001", Data: data.Bytes(), MediaType: "image/png"}
			d := daemon(t)
			ag := paseotest.Agent("a1", "w1", "running")
			ag["cwd"] = dir
			if behavior == "queue" {
				ag["cwd"] = ""
				workspace := paseotest.Workspace("w1", "directory", "running")
				workspace["workspaceDirectory"] = dir
				d.SetWorkspaces(workspace)
			}
			d.SetAgents(ag)
			w := watcher(t, d)
			eventually(t, w, "agent", func(s PaseoStatus) bool { return len(s.Agents) == 1 })
			args, _ := json.Marshal(map[string]any{"agentId": "a1", "text": "Inspect " + img.Path(), "behavior": behavior, "attachments": []attachment.Image{img}})
			done := make(chan error, 1)
			go func() { _, err := execute(t, w, SendMessageKind, string(args)); done <- err }()
			if behavior == "queue" {
				deadline := time.Now().Add(time.Second)
				for {
					w.mu.Lock()
					waiting := len(w.queues["a1"]) > 0
					w.mu.Unlock()
					if waiting {
						break
					}
					select {
					case err := <-done:
						t.Fatalf("queue failed: %v", err)
					default:
					}
					if time.Now().After(deadline) {
						t.Fatal("message never queued")
					}
					time.Sleep(time.Millisecond)
				}
				if len(sentTexts(d)) != 0 {
					t.Fatal("sent before turn ended")
				}
				if _, err := os.Stat(filepath.Join(dir, img.Path())); !os.IsNotExist(err) {
					t.Fatalf("queued image written before delivery: %v", err)
				}
				ag["status"] = "idle"
				d.Push("agent_update", map[string]any{"kind": "upsert", "agent": ag})
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(dir, img.Path()))
			if err != nil || !bytes.Equal(got, img.Data) {
				t.Fatalf("message image: %v", err)
			}
			req := d.Requests("send_agent_message_request")[0]
			want := behavior
			if want == "queue" {
				want = "steer"
			}
			if req["activeTurnBehavior"] != want || req["text"] != "Inspect "+img.Path() {
				t.Fatalf("sent message: %v", req)
			}
			// A failed delivery must not send the message, including a queued send.
			os.Remove(filepath.Join(dir, img.Path()))
			os.RemoveAll(dir)
			if behavior == "queue" {
				w.mu.Lock()
				w.settling["a1"] = time.Time{}
				w.mu.Unlock()
				ag["status"] = "idle"
				d.Push("agent_update", map[string]any{"kind": "upsert", "agent": ag})
				eventually(t, w, "idle again", func(s PaseoStatus) bool { return len(s.Agents) == 1 && s.Agents[0].Status == "idle" })
			}
			args, _ = json.Marshal(map[string]any{"agentId": "a1", "text": "Inspect " + img.Path(), "behavior": behavior, "attachments": []attachment.Image{img}})
			if _, err := execute(t, w, SendMessageKind, string(args)); err == nil {
				t.Fatal("sent without writing image")
			}
			if len(sentTexts(d)) != 1 {
				t.Fatal("sent despite image delivery failure")
			}
		})
	}
}
