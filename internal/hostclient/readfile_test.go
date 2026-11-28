package hostclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseCommandReadFile(t *testing.T) {
	t.Parallel()

	good := `{"path":"/workspace/wt/work/brief.md","roots":["/workspace/wt","/workspace/repo"]}`
	parsed, err := ParseCommand(ReadFileKind, json.RawMessage(good))
	if err != nil {
		t.Fatalf("ParseCommand valid read_file: %v", err)
	}
	a, ok := parsed.(ReadFileArgs)
	if !ok {
		t.Fatalf("parsed type = %T, want ReadFileArgs", parsed)
	}
	if a.Path != "/workspace/wt/work/brief.md" || len(a.Roots) != 2 {
		t.Fatalf("parsed = %+v", a)
	}

	badCases := []string{
		`{}`,
		`{"path":"relative/file.md","roots":["/workspace/wt"]}`,
		`{"path":"/workspace/wt/../secret.txt","roots":["/workspace/wt"]}`,
		`{"path":"/workspace/wt/file.md","roots":[]}`,
		`{"path":"/workspace/wt/file.md","roots":["/"]}`,
		`{"path":"/workspace/wt/file.md","roots":["relative"]}`,
		`{"path":"/workspace/wt/file.md","roots":["/workspace/../etc"]}`,
		`{"path":"/workspace/wt/file.md","roots":["/workspace/wt"],"extra":true}`,
	}
	for _, raw := range badCases {
		if _, err := ParseCommand(ReadFileKind, json.RawMessage(raw)); err == nil {
			t.Errorf("ParseCommand accepted unsafe read_file args: %s", raw)
		}
	}
}

func TestReadFileWithinRootsAndSecurity(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	wt := filepath.Join(base, "wt")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(filepath.Join(wt, "work", "2026-11-27"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}

	mdPath := filepath.Join(wt, "work", "2026-11-27", "brief.md")
	if err := os.WriteFile(mdPath, []byte("# Brief\n\nHello world.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pngBytes := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d}
	pngPath := filepath.Join(wt, "shot.png")
	if err := os.WriteFile(pngPath, pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	secretPath := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secretPath, []byte("top-secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	escapeLink := filepath.Join(wt, "escape.txt")
	if err := os.Symlink(secretPath, escapeLink); err != nil {
		t.Fatal(err)
	}
	bigPath := filepath.Join(wt, "big.bin")
	if err := os.WriteFile(bigPath, make([]byte, MaxReadFileBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	// 1. Valid markdown file
	res, err := ReadFile(ctx, ReadFileArgs{Path: mdPath, Roots: []string{wt}})
	if err != nil {
		t.Fatalf("ReadFile markdown: %v", err)
	}
	if res.Encoding != "utf-8" || res.Content != "# Brief\n\nHello world.\n" || !strings.HasPrefix(res.MediaType, "text/markdown") || res.Size != 22 {
		t.Fatalf("markdown result = %+v", res)
	}

	// 2. Valid binary PNG file (base64)
	res, err = ReadFile(ctx, ReadFileArgs{Path: pngPath, Roots: []string{wt}})
	if err != nil {
		t.Fatalf("ReadFile png: %v", err)
	}
	if res.Encoding != "base64" || res.MediaType != "image/png" || res.Size != int64(len(pngBytes)) {
		t.Fatalf("png result = %+v", res)
	}
	dec, err := base64.StdEncoding.DecodeString(res.Content)
	if err != nil || string(dec) != string(pngBytes) {
		t.Fatalf("decoded png mismatch: %v", err)
	}

	// 3. Outside root refused
	if _, err := ReadFile(ctx, ReadFileArgs{Path: secretPath, Roots: []string{wt}}); err == nil {
		t.Fatal("ReadFile allowed file outside root")
	}

	// 4. Symlink escaping root refused
	if _, err := ReadFile(ctx, ReadFileArgs{Path: escapeLink, Roots: []string{wt}}); err == nil {
		t.Fatal("ReadFile allowed symlink escaping root")
	}

	// 5. Directory refused
	if _, err := ReadFile(ctx, ReadFileArgs{Path: filepath.Join(wt, "work"), Roots: []string{wt}}); err == nil {
		t.Fatal("ReadFile allowed directory")
	}

	// 6. Missing file returns not found
	if _, err := ReadFile(ctx, ReadFileArgs{Path: filepath.Join(wt, "nope.md"), Roots: []string{wt}}); err == nil || !strings.Contains(strings.ToLower(err.Error()), "not found") {
		t.Fatalf("missing file err = %v, want not found", err)
	}

	// 7. Oversized file refused
	if _, err := ReadFile(ctx, ReadFileArgs{Path: bigPath, Roots: []string{wt}}); err == nil || !strings.Contains(strings.ToLower(err.Error()), "large") {
		t.Fatalf("oversized file err = %v, want too large", err)
	}
}
