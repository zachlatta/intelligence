// Package attachment defines the shared file/image validation and task-reference format.
package attachment

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"unicode"
)

// Limits on one task's or message's attachments; attachments.js and the README repeat them.
const (
	MaxImage  = 4 << 20    // bytes in one attachment
	MaxTotal  = 8 << 20    // bytes in all of one task's or message's attachments
	MaxCount  = 8          // attachments in one task or message
	MaxPixels = 40_000_000 // pixels in one PNG/JPEG/GIF image, so decoding one stays bounded
)

// Reference is persisted in task or reply text, so later workflow commands resolve the same attachments.
var Reference = regexp.MustCompile(`\[attachment:([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\]`)

// Image is an attachment sent over the authenticated host stream, never to the model as base64.
type Image struct {
	ID        string `json:"id"`
	Data      []byte `json:"data"`
	MediaType string `json:"mediaType"`
	Name      string `json:"name,omitempty"`
}

// extMediaTypes is deterministic across OSes so server and host agree on mediaType.
var extMediaTypes = map[string]string{
	"pdf":      "application/pdf",
	"txt":      "text/plain",
	"log":      "text/plain",
	"md":       "text/markdown",
	"markdown": "text/markdown",
	"csv":      "text/csv",
	"tsv":      "text/tab-separated-values",
	"json":     "application/json",
	"xml":      "application/xml",
	"html":     "text/html",
	"htm":      "text/html",
	"css":      "text/css",
	"js":       "text/javascript",
	"svg":      "image/svg+xml",
	"webp":     "image/webp",
	"yaml":     "text/yaml",
	"yml":      "text/yaml",
	"zip":      "application/zip",
	"gz":       "application/gzip",
	"tar":      "application/x-tar",
}

var mediaTypeExts = map[string]string{
	"image/png":                 "png",
	"image/jpeg":                "jpg",
	"image/gif":                 "gif",
	"image/webp":                "webp",
	"image/svg+xml":             "svg",
	"application/pdf":           "pdf",
	"text/plain":                "txt",
	"text/markdown":             "md",
	"text/csv":                  "csv",
	"text/tab-separated-values": "tsv",
	"application/json":          "json",
	"application/xml":           "xml",
	"text/html":                 "html",
	"text/css":                  "css",
	"text/javascript":           "js",
	"text/yaml":                 "yaml",
	"application/zip":           "zip",
	"application/gzip":          "gz",
	"application/x-tar":         "tar",
}

// ValidateFile checks data within the limits, decodes PNG/JPEG/GIF images to guard against
// corrupt or oversized pixel dimensions, and returns a deterministic media type.
func ValidateFile(name string, data []byte) (string, error) {
	if len(data) == 0 || len(data) > MaxImage {
		return "", fmt.Errorf("attachments must be between 1 byte and 4 MiB")
	}
	detected, _, _ := mime.ParseMediaType(http.DetectContentType(data))
	switch detected {
	case "image/png", "image/jpeg", "image/gif":
		cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > MaxPixels {
			return "", fmt.Errorf("use a valid PNG, JPEG or GIF image up to 40 megapixels")
		}
		if _, _, err = image.Decode(bytes.NewReader(data)); err != nil {
			return "", fmt.Errorf("invalid image: %w", err)
		}
		switch format {
		case "png":
			return "image/png", nil
		case "jpeg":
			return "image/jpeg", nil
		case "gif":
			return "image/gif", nil
		}
		return "", fmt.Errorf("unsupported image type")
	}
	if detected == "" || detected == "application/octet-stream" || detected == "text/plain" || detected == "text/xml" {
		if ext := safeExtFromName(name); ext != "" {
			if byExt, ok := extMediaTypes[ext]; ok {
				return byExt, nil
			}
		}
	}
	if detected == "" {
		detected = "application/octet-stream"
	}
	return detected, nil
}

// IsImage reports whether mediaType is a raster image type.
func IsImage(mediaType string) bool {
	switch mediaType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

// Path is where the host writes i's delivery copy, relative to the agent's working directory.
func (i Image) Path() string {
	ext := ""
	if IsImage(i.MediaType) {
		ext = mediaTypeExts[i.MediaType]
	}
	if ext == "" {
		ext = safeExtFromName(i.Name)
	}
	if ext == "" {
		ext = mediaTypeExts[i.MediaType]
	}
	if ext == "" {
		ext = "bin"
	}
	return ".factory-scratch/attachments/" + i.ID + "." + ext
}

// SafeDisplayName returns a single-line, prompt-safe basename for display in agent prompts.
func SafeDisplayName(name string) string {
	name = strings.TrimSpace(name)
	if idx := strings.LastIndexAny(name, `/\`); idx >= 0 {
		name = name[idx+1:]
	}
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || r == 0x7f || r == ';' || !unicode.IsPrint(r) {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
		if b.Len() >= 120 {
			break
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func safeExtFromName(name string) string {
	name = strings.TrimSpace(name)
	if idx := strings.LastIndexAny(name, `/\`); idx >= 0 {
		name = name[idx+1:]
	}
	dot := strings.LastIndexByte(name, '.')
	if dot <= 0 || dot == len(name)-1 {
		return ""
	}
	raw := strings.ToLower(name[dot+1:])
	if len(raw) > 16 {
		return ""
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
			return ""
		}
	}
	return raw
}
