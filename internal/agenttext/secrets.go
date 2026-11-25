// Package agenttext refuses credentials before text enters an agent transcript.
package agenttext

import (
	"errors"
	"regexp"
)

// PDW access keys, Intelligence host keys, MCP keys and workflow API keys. The
// Intelligence keys contain 32 random bytes encoded as unpadded URL-safe base64 (43 characters).
var secret = regexp.MustCompile(`pdw_[a-z0-9]{12,24}_[A-Za-z0-9_-]{30,}|(?:ibk_|imcp_|iwfk_)[A-Za-z0-9_-]{43,}`)

// ErrCredential identifies a permanent refusal without exposing the matched text.
var ErrCredential = errors.New("refusing agent text containing a credential; pass an environment variable name or a 0600 file path instead")

func Validate(text string) error {
	if secret.MatchString(text) {
		return ErrCredential
	}
	return nil
}

func Checked(text string) (string, error) {
	if err := Validate(text); err != nil {
		return "", err
	}
	return text, nil
}
