package agenttext

import (
	"errors"
	"strings"
	"testing"
)

func TestCredentialPatternsAndSafeReferences(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		reject     bool
	}{
		{"PDW minimum", "pdw_" + strings.Repeat("a", 12) + "_" + strings.Repeat("x", 30), true},
		{"PDW maximum ID", "pdw_" + strings.Repeat("z", 24) + "_" + strings.Repeat("X_-", 20), true},
		{"host key", "ibk_" + strings.Repeat("a", 43), true},
		{"MCP key", "imcp_" + strings.Repeat("b", 43), true},
		{"short non-key", "pdw_" + strings.Repeat("a", 11) + "_" + strings.Repeat("x", 30), false},
		{"env reference", "Read PDW_TOKEN_FILE from your environment", false},
		{"file reference", "Use the 0600 file .factory-scratch/access-key", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Checked(tc.text)
			if errors.Is(err, ErrCredential) != tc.reject {
				t.Fatal("incorrect credential classification")
			}
			if tc.reject {
				if got != "" || strings.Contains(err.Error(), tc.text) {
					t.Fatal("refusal disclosed the credential")
				}
			} else if err != nil || got != tc.text {
				t.Fatal("safe reference changed")
			}
		})
	}
}
