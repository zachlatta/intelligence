package hostclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"
)

// MaxReadFileBytes caps read_file responses so even base64-encoded files stay well within MaxFrame.
const MaxReadFileBytes = 2 << 20

// ReadFileArgs reads a single regular file on the host, strictly inside one of Roots
// (after resolving symlinks). Used by Grimm to view file:// links from a session.
type ReadFileArgs struct {
	Path  string   `json:"path"`
	Roots []string `json:"roots"`
}

func validateReadFileArgs(a ReadFileArgs) (ReadFileArgs, error) {
	p := strings.TrimSpace(a.Path)
	if p == "" || !path.IsAbs(p) || strings.ContainsRune(p, 0) || slices.Contains(strings.Split(p, "/"), "..") {
		return ReadFileArgs{}, errors.New("read_file needs a clean absolute file path")
	}
	cleanPath := path.Clean(p)
	if cleanPath == "/" {
		return ReadFileArgs{}, errors.New("read_file path cannot be /")
	}
	if len(a.Roots) == 0 || len(a.Roots) > 16 {
		return ReadFileArgs{}, errors.New("read_file needs 1 to 16 allowed root directories")
	}
	roots := make([]string, 0, len(a.Roots))
	for _, r := range a.Roots {
		r = strings.TrimSpace(r)
		if r == "" || !path.IsAbs(r) || strings.ContainsRune(r, 0) || slices.Contains(strings.Split(r, "/"), "..") {
			return ReadFileArgs{}, errors.New("read_file roots must be clean absolute directories")
		}
		cr := path.Clean(r)
		if cr == "/" {
			return ReadFileArgs{}, errors.New("read_file root cannot be /")
		}
		if !slices.Contains(roots, cr) {
			roots = append(roots, cr)
		}
	}
	return ReadFileArgs{Path: cleanPath, Roots: roots}, nil
}

// ReadFile reads a single regular file at a.Path if and only if its symlink-resolved real path
// lives strictly inside one of the symlink-resolved directories in a.Roots.
func ReadFile(ctx context.Context, a ReadFileArgs) (CommandResult, error) {
	if err := ctx.Err(); err != nil {
		return CommandResult{}, err
	}
	a, err := validateReadFileArgs(a)
	if err != nil {
		return CommandResult{}, err
	}

	resolvedRoots := make([]string, 0, len(a.Roots))
	for _, r := range a.Roots {
		realRoot, err := filepath.EvalSymlinks(r)
		if err != nil {
			continue
		}
		info, err := os.Stat(realRoot)
		if err != nil || !info.IsDir() {
			continue
		}
		resolvedRoots = append(resolvedRoots, filepath.Clean(realRoot))
	}
	if len(resolvedRoots) == 0 {
		return CommandResult{}, errors.New("session workspace directory not found on host")
	}

	realPath, err := filepath.EvalSymlinks(a.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return CommandResult{}, errors.New("file not found")
		}
		return CommandResult{}, fmt.Errorf("could not resolve file: %w", err)
	}
	realPath = filepath.Clean(realPath)

	allowed := false
	for _, root := range resolvedRoots {
		if pathWithinRoot(root, realPath) {
			allowed = true
			break
		}
	}
	if !allowed {
		return CommandResult{}, errors.New("path is outside the session workspace")
	}

	f, err := os.Open(realPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return CommandResult{}, errors.New("file not found")
		}
		return CommandResult{}, fmt.Errorf("could not open file: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return CommandResult{}, fmt.Errorf("could not stat file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return CommandResult{}, errors.New("path is not a regular file")
	}
	if info.Size() > MaxReadFileBytes {
		return CommandResult{}, fmt.Errorf("file is too large to view (%d bytes, max %d bytes)", info.Size(), MaxReadFileBytes)
	}

	data, err := io.ReadAll(io.LimitReader(f, MaxReadFileBytes+1))
	if err != nil {
		return CommandResult{}, fmt.Errorf("could not read file: %w", err)
	}
	if len(data) > MaxReadFileBytes {
		return CommandResult{}, fmt.Errorf("file is too large to view (max %d bytes)", MaxReadFileBytes)
	}

	mediaType, preferText := detectFileMediaType(realPath, data)
	if preferText && utf8.Valid(data) && !bytes.ContainsRune(data, 0) {
		return CommandResult{
			Content:   string(data),
			Encoding:  "utf-8",
			MediaType: mediaType,
			Size:      int64(len(data)),
		}, nil
	}
	return CommandResult{
		Content:   base64.StdEncoding.EncodeToString(data),
		Encoding:  "base64",
		MediaType: mediaType,
		Size:      int64(len(data)),
	}, nil
}

func pathWithinRoot(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func detectFileMediaType(filePath string, data []byte) (mediaType string, preferText bool) {
	ext := strings.ToLower(filepath.Ext(filePath))
	switch ext {
	case ".md", ".markdown", ".mdx":
		return "text/markdown; charset=utf-8", true
	case ".json", ".jsonl":
		return "application/json; charset=utf-8", true
	case ".yaml", ".yml":
		return "text/yaml; charset=utf-8", true
	case ".csv":
		return "text/csv; charset=utf-8", true
	case ".html", ".htm":
		return "text/html; charset=utf-8", true
	case ".svg":
		return "image/svg+xml", true
	case ".xml":
		return "application/xml; charset=utf-8", true
	case ".txt", ".log", ".go", ".js", ".mjs", ".cjs", ".ts", ".tsx", ".jsx", ".css", ".scss",
		".py", ".rb", ".rs", ".c", ".h", ".cc", ".cpp", ".hpp", ".java", ".kt", ".swift",
		".sh", ".bash", ".zsh", ".fish", ".sql", ".toml", ".ini", ".cfg", ".conf", ".env",
		".diff", ".patch", ".makefile", ".dockerfile", ".gitignore", ".editorconfig":
		return "text/plain; charset=utf-8", true
	case ".png":
		return "image/png", false
	case ".jpg", ".jpeg":
		return "image/jpeg", false
	case ".gif":
		return "image/gif", false
	case ".webp":
		return "image/webp", false
	case ".pdf":
		return "application/pdf", false
	}
	if len(data) == 0 {
		return "text/plain; charset=utf-8", true
	}
	detected := http.DetectContentType(data)
	if strings.HasPrefix(detected, "text/") {
		return detected, true
	}
	if utf8.Valid(data) && !bytes.ContainsRune(data, 0) {
		return "text/plain; charset=utf-8", true
	}
	return detected, false
}
