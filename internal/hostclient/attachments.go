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

// Canonical bytes live in Postgres. These private host copies are recreated on delivery.
func materializeImages(cwd string, images []attachment.Image) error {
	total := 0
	for _, img := range images {
		if len(img.ID) != 36 || !attachment.Reference.MatchString("[attachment:"+img.ID+"]") {
			return fmt.Errorf("invalid attachment ID")
		}
		media, err := attachment.Validate(img.Data)
		if err != nil {
			return err
		}
		if media != img.MediaType {
			return fmt.Errorf("image type mismatch")
		}
		total += len(img.Data)
		if total > attachment.MaxTotal || len(images) > attachment.MaxCount {
			return fmt.Errorf("too many image attachments")
		}
		// Root prevents a repository's symlink from redirecting writes outside its working directory.
		root, err := os.OpenRoot(cwd)
		if err != nil {
			return err
		}
		err = root.MkdirAll(".factory-scratch/attachments", 0700)
		if err == nil {
			var f *os.File
			f, err = root.OpenFile(filepath.FromSlash(img.Path()), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if errors.Is(err, os.ErrExist) {
				// Accept an identical delivery copy, but never overwrite a repository file.
				var old []byte
				var existing *os.File
				existing, err = root.Open(filepath.FromSlash(img.Path()))
				if err == nil {
					old, err = io.ReadAll(io.LimitReader(existing, int64(len(img.Data))+1))
					existing.Close()
				}
				if err == nil && !bytes.Equal(old, img.Data) {
					err = fmt.Errorf("attachment path already exists with different contents")
				}
				root.Close()
				if err != nil {
					return err
				}
				continue
			}
			if err == nil {
				_, err = f.Write(img.Data)
				closeErr := f.Close()
				if err == nil {
					err = closeErr
				}
			}
		}
		root.Close()
		if err != nil {
			return fmt.Errorf("write image attachment: %w", err)
		}
	}
	return nil
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
