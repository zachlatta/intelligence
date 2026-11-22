package attachment

import (
	"bytes"
	"image"
	"image/png"
	"strings"
	"testing"
)

func TestValidateFileAndPath(t *testing.T) {
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		fileName  string
		data      []byte
		wantMedia string
		wantExt   string
	}{
		{"png", "screen.png", pngBuf.Bytes(), "image/png", ".png"},
		{"pdf", "report.pdf", []byte("%PDF-1.4\n1 0 obj\n<<>>\nendobj\n"), "application/pdf", ".pdf"},
		{"text", "notes.txt", []byte("hello world\n"), "text/plain", ".txt"},
		{"csv", "data.csv", []byte("a,b\n1,2\n"), "text/csv", ".csv"},
		{"json", "config.json", []byte(`{"ok":true}`), "application/json", ".json"},
		{"svg", "diagram.svg", []byte("<svg></svg>"), "image/svg+xml", ".svg"},
		{"binary", "blob.dat", []byte{0x00, 0x01, 0x02, 0x03}, "application/octet-stream", ".dat"},
		{"traversal name", "../../etc/passwd.log", []byte("line 1\n"), "text/plain", ".log"},
		{"no extension text", "Makefile", []byte("all:\n\techo hi\n"), "text/plain", ".txt"},
		{"no extension bin", "rawdata", []byte{0x00, 0x01, 0x02, 0x03}, "application/octet-stream", ".bin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			media, err := ValidateFile(tc.fileName, tc.data)
			if err != nil {
				t.Fatalf("ValidateFile(%q): %v", tc.fileName, err)
			}
			if media != tc.wantMedia {
				t.Fatalf("media = %q, want %q", media, tc.wantMedia)
			}
			img := Image{ID: "00000000-0000-4000-8000-000000000001", Name: tc.fileName, MediaType: media, Data: tc.data}
			path := img.Path()
			wantPath := ".factory-scratch/attachments/00000000-0000-4000-8000-000000000001" + tc.wantExt
			if path != wantPath {
				t.Fatalf("Path() = %q, want %q", path, wantPath)
			}
		})
	}
}

func TestValidateFileRejectsInvalid(t *testing.T) {
	if _, err := ValidateFile("empty.txt", nil); err == nil {
		t.Fatal("accepted empty file")
	}
	if _, err := ValidateFile("big.bin", make([]byte, MaxImage+1)); err == nil {
		t.Fatal("accepted oversized file")
	}
	// Corrupt PNG (valid PNG signature, truncated body) must still be rejected.
	corruptPNG := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0}
	if _, err := ValidateFile("corrupt.png", corruptPNG); err == nil {
		t.Fatal("accepted corrupt PNG")
	}
}

func TestSafeDisplayName(t *testing.T) {
	got := SafeDisplayName("  ../dir/my report;\n(v1).pdf \x00 ")
	if strings.ContainsAny(got, "\n\r\x00;") {
		t.Fatalf("unsafe display name: %q", got)
	}
	if !strings.Contains(got, "my report") || !strings.HasSuffix(got, ".pdf") {
		t.Fatalf("unexpected display name: %q", got)
	}
}
