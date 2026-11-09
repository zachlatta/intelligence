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

const MaxImage = 4 << 20
const MaxTotal = 8 << 20
const MaxCount = 8

// Reference is persisted in task text, so later workflow commands resolve the same images.
var Reference = regexp.MustCompile(`\[attachment:([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\]`)

// Image is sent over the authenticated host stream, never to the model as base64.
type Image struct {
	ID        string `json:"id"`
	Data      []byte `json:"data"`
	MediaType string `json:"mediaType"`
}

func Validate(data []byte) (string, error) {
	if len(data) == 0 || len(data) > MaxImage {
		return "", fmt.Errorf("images must be between 1 byte and 4 MiB")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > 40000000 {
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

func (i Image) Path() string {
	ext := map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/gif": "gif"}[i.MediaType]
	return ".factory-scratch/attachments/" + i.ID + "." + ext
}
