// Package attachment defines the shared image validation and task-reference format.
package attachment

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"regexp"
)

// Limits on one task's or message's images; attachments.js and the README repeat them.
const (
	MaxImage  = 4 << 20    // bytes in one image
	MaxTotal  = 8 << 20    // bytes in all of one task's or message's images
	MaxCount  = 8          // images in one task or message
	MaxPixels = 40_000_000 // pixels in one image, so decoding one stays bounded
)

// Reference is persisted in task text, so later workflow commands resolve the same images.
var Reference = regexp.MustCompile(`\[attachment:([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\]`)

// Image is sent over the authenticated host stream, never to the model as base64.
type Image struct {
	ID        string `json:"id"`
	Data      []byte `json:"data"`
	MediaType string `json:"mediaType"`
}

// Validate decodes data as a PNG, JPEG or GIF within the limits and returns its media type.
func Validate(data []byte) (string, error) {
	if len(data) == 0 || len(data) > MaxImage {
		return "", fmt.Errorf("images must be between 1 byte and 4 MiB")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > MaxPixels {
		return "", fmt.Errorf("use a PNG, JPEG or GIF image up to 40 megapixels")
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

// Path is where the host writes i's delivery copy, relative to the agent's working directory.
func (i Image) Path() string {
	ext := map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/gif": "gif"}[i.MediaType]
	return ".factory-scratch/attachments/" + i.ID + "." + ext
}
