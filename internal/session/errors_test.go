package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMatchesCredentialsExpired(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		stderr string
		want   bool
	}{
		{"empty", "", false},
		{"unrelated error", "An error occurred (AccessDenied) when calling the StartSession operation", false},
		{"sso token load", "Error loading SSO Token: Token for https://example.awsapps.com has expired", true},
		{"sso session expired", "The SSO session associated with this profile has expired or is otherwise invalid", true},
		{"refresh failed", "Token has expired and refresh failed", true},
		{"sts expired token", "An error occurred (ExpiredTokenException) when calling the StartSession operation", true},
		{"security token expired", "the security token included in the request is expired", true},
		{"short-term creds", "Your short-term credentials have expired. Please refresh.", true},
		{"case insensitive", "error loading sso token: token has expired", true},
		{"pattern mid-stream", strings.Repeat("noise\n", 50) + "ExpiredTokenException\n" + strings.Repeat("noise\n", 50), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, matchesCredentialsExpired([]byte(tt.stderr)))
		})
	}
}

func TestExpiredSnippet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		stderr string
		want   string
	}{
		{"no match", "nothing interesting", ""},
		{"single line", "Error loading SSO Token: expired", "Error loading SSO Token: expired"},
		{"pattern in middle line", "prelude\nExpiredTokenException occurred\npostlude", "ExpiredTokenException occurred"},
		{"trailing whitespace", "  Token has expired and refresh failed  \n", "Token has expired and refresh failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, expiredSnippet([]byte(tt.stderr)))
		})
	}
}
