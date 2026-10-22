package session

import (
	"errors"
	"strings"
)

// ErrCredentialsExpired is wrapped by Session.Wait when aws-cli stderr shows an
// expired SSO session or token; no retry can succeed until the user logs in again.
var ErrCredentialsExpired = errors.New("aws credentials expired")

// ErrTargetNotConnected is wrapped by Session.Wait when SSM reports the instance
// gone (terminated, agent offline, rotated out of its ASG): re-resolve before retrying.
var ErrTargetNotConnected = errors.New("aws ssm target not connected")

// Several phrasings for one condition: the AWS CLI rewords this across versions.
var expiredPatterns = []string{
	"Error loading SSO Token",
	"The SSO session associated with this profile has expired",
	"Token has expired and refresh failed",
	"ExpiredTokenException",
	"the security token included in the request is expired",
	"Your short-term credentials have expired",
}

// targetNotConnectedPatterns lists stderr substrings that identify the
// AWS SSM "TargetNotConnected" error. The error code is the canonical
// signal; the message wording ("i-xxx is not connected") is the common form.
var targetNotConnectedPatterns = []string{
	"TargetNotConnected",
}

// matchesCredentialsExpired reports whether the given stderr bytes contain any
// of the known expired-credentials patterns.
func matchesCredentialsExpired(stderr []byte) bool {
	return containsAnyPattern(stderr, expiredPatterns)
}

// expiredSnippet returns the first line of stderr that contains a known
// expired-credentials pattern, trimmed — used as diagnostic detail in the
// wrapped error and in emitted events.
func expiredSnippet(stderr []byte) string {
	return firstMatchingLine(stderr, expiredPatterns)
}

// matchesTargetNotConnected reports whether the given stderr bytes contain the
// SSM TargetNotConnected signal.
func matchesTargetNotConnected(stderr []byte) bool {
	return containsAnyPattern(stderr, targetNotConnectedPatterns)
}

// targetNotConnectedSnippet returns the first line of stderr that identifies a
// TargetNotConnected error, trimmed.
func targetNotConnectedSnippet(stderr []byte) string {
	return firstMatchingLine(stderr, targetNotConnectedPatterns)
}

func containsAnyPattern(stderr []byte, patterns []string) bool {
	if len(stderr) == 0 {
		return false
	}
	lower := strings.ToLower(string(stderr))
	for _, p := range patterns {
		if strings.Contains(lower, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

func firstMatchingLine(stderr []byte, patterns []string) string {
	lower := strings.ToLower(string(stderr))
	for _, p := range patterns {
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
