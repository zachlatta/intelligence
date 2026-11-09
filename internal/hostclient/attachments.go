package hostclient

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/zachlatta/intelligence/internal/attachment"
)

// materializeImages writes images' delivery copies under cwd (at Image.Path). Canonical bytes live
// in Postgres; these private host copies are recreated on each delivery.
func materializeImages(cwd string, images []attachment.Image) error {
	if len(images) == 0 {
		return nil
	}
	if len(images) > attachment.MaxCount {
		return fmt.Errorf("too many image attachments")
	}
	total := 0
	for _, img := range images {
		if !attachment.Reference.MatchString("[attachment:" + img.ID + "]") {
			return fmt.Errorf("invalid attachment ID")
		}
		media, err := attachment.Validate(img.Data)
		if err != nil {
			return err
		}
		if media != img.MediaType {
			return fmt.Errorf("image type mismatch")
		}
		if total += len(img.Data); total > attachment.MaxTotal {
			return fmt.Errorf("too many image attachments")
		}
	}
	// Root keeps a repository's symlink from redirecting writes outside its working directory.
	root, err := os.OpenRoot(cwd)
	if err != nil {
		return fmt.Errorf("write image attachment: %w", err)
	}
	defer root.Close()
	if err := root.MkdirAll(".factory-scratch/attachments", 0700); err != nil {
		return fmt.Errorf("write image attachment: %w", err)
	}
	for _, img := range images {
		if err := writeImage(root, img); err != nil {
			return fmt.Errorf("write image attachment: %w", err)
		}
	}
	return nil
}

// writeImage creates img's copy in root, accepting an identical copy but never overwriting a file.
func writeImage(root *os.Root, img attachment.Image) error {
	path := filepath.FromSlash(img.Path())
	f, err := root.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		existing, err := root.Open(path)
		if err != nil {
			return err
		}
		defer existing.Close()
		old, err := io.ReadAll(io.LimitReader(existing, int64(len(img.Data))+1))
		if err != nil {
			return err
		}
		if !bytes.Equal(old, img.Data) {
			return fmt.Errorf("attachment path already exists with different contents")
		}
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := f.Write(img.Data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Resolve the session's current directory when actually sending, including after a queue wait.
func (w *Watcher) prepareMessageImages(a AgentArgs) error {
	if len(a.Attachments) == 0 {
		return nil
	}
	w.mu.Lock()
	ag, ok := w.agents[a.AgentID]
	cwd := ag.Cwd
	if cwd == "" {
		cwd = w.workspaces[ag.WorkspaceID].Directory
	}
	w.mu.Unlock()
	if !ok || cwd == "" {
		return fmt.Errorf("cannot locate agent %s for image delivery", a.AgentID)
	}
	home, _ := os.UserHomeDir()
	return materializeImages(expandHome(cwd, home), a.Attachments)
}
