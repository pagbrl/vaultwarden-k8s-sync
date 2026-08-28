package vaultwarden

import (
	"bufio"
	"strings"
)

// ParseNotes extracts KEY=VALUE pairs from a secure-note body.
//
// Rules:
//   - Lines are split on the first '=' only, so values may themselves contain
//     '=' characters (e.g. base64 padding, connection strings).
//   - A leading "export " prefix is stripped (shell dotenv style).
//   - Blank lines and lines whose first non-space character is '#' are skipped.
//   - Leading/trailing whitespace around the key is trimmed. The value is taken
//     verbatim after the '=' (only a trailing '\r' from CRLF input is removed),
//     because secret values may intentionally contain spaces.
//   - Later occurrences of the same key override earlier ones.
//
// A line with no '=' after optional "export " stripping is ignored.
func ParseNotes(body string) map[string]string {
	out := make(map[string]string)

	scanner := bufio.NewScanner(strings.NewReader(body))
	// Allow long lines (default token size is 64KiB which is plenty, but be
	// explicit for very long secret values).
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		// Normalise CRLF: bufio.Scanner strips '\n' but leaves a trailing '\r'.
		line = strings.TrimSuffix(line, "\r")

		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		// Strip an optional leading "export " (only when it's a real prefix).
		trimmed = strings.TrimPrefix(trimmed, "export ")

		eq := strings.IndexByte(trimmed, '=')
		if eq < 0 {
			continue
		}

		key := strings.TrimSpace(trimmed[:eq])
		if key == "" {
			continue
		}
		value := trimmed[eq+1:]

		out[key] = value
	}

	return out
}
