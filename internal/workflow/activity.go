package workflow

import (
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

func activityValues(m Move) (string, *time.Time, error) {
	line := strings.TrimSpace(m.Now)
	if line == "" || utf8.RuneCountInString(line) > 280 || strings.ContainsFunc(m.Now, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) || unicode.Is(unicode.Cf, r)
	}) {
		return "", nil, bad("current activity must be one visible line of 1–280 characters")
	}
	var eta *time.Time
	if m.ETA != "" {
		at, err := time.Parse(time.RFC3339, m.ETA)
		if err != nil {
			return "", nil, bad("eta must be RFC3339")
		}
		eta = &at
	}
	return line, eta, nil
}
