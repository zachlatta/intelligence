package hostclient

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/zachlatta/intelligence/internal/attachment"
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
