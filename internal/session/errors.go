package session

import (
	"errors"
	"strings"
)

// ErrCredentialsExpired signals that aws ssm start-session failed because the
// underlying AWS credentials (typically an SSO session) are no longer valid.
// When returned from Session.Wait, the engine halts reconnect attempts and
// emits a CredentialsExpired event so the user can re-authenticate.
var ErrCredentialsExpired = errors.New("aws credentials expired")

// expiredPatterns lists stderr substrings that indicate expired credentials.
// Matching is case-insensitive. The AWS CLI wording varies across versions —
// the matched snippet is preserved in the wrapped error so drift can be
// diagnosed from event telemetry rather than source diving.
var expiredPatterns = []string{
	"Error loading SSO Token",
	"The SSO session associated with this profile has expired",
	"Token has expired and refresh failed",
	"ExpiredTokenException",
	"the security token included in the request is expired",
	"Your short-term credentials have expired",
}

// matchesCredentialsExpired reports whether the given stderr bytes contain any
// of the known expired-credentials patterns.
func matchesCredentialsExpired(stderr []byte) bool {
	if len(stderr) == 0 {
		return false
	}
	lower := strings.ToLower(string(stderr))
	for _, p := range expiredPatterns {
		if strings.Contains(lower, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

// expiredSnippet returns the first line of stderr that contains a known
// expired-credentials pattern, trimmed — used as diagnostic detail in the
// wrapped error and in emitted events.
func expiredSnippet(stderr []byte) string {
	lower := strings.ToLower(string(stderr))
	for _, p := range expiredPatterns {
		idx := strings.Index(lower, strings.ToLower(p))
		if idx < 0 {
			continue
		}
		start := strings.LastIndexByte(lower[:idx], '\n') + 1
		end := strings.IndexByte(lower[idx:], '\n')
		if end < 0 {
			return strings.TrimSpace(string(stderr[start:]))
		}
		return strings.TrimSpace(string(stderr[start : idx+end]))
	}
	return ""
}
