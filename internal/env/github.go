package env

import (
	"fmt"
	"strings"
	"unicode"
)

// KeyGitHubToken names the token the publisher's license detection sends to
// the GitHub REST API; the client never reads it.
const KeyGitHubToken = "GITHUB_TOKEN" //nolint:gosec // G101: the name of the variable, not a credential

// GitHubToken returns the token from GITHUB_TOKEN without surrounding
// whitespace. A token containing whitespace or control characters is an
// error: it cannot be sent in an HTTP header, and silently dropping it would
// fall back to the much lower anonymous rate limit.
func GitHubToken() (token string, set bool, err error) {
	raw, ok := nonEmpty(KeyGitHubToken)
	if !ok {
		return "", false, nil
	}

	token = strings.TrimSpace(raw)
	if strings.ContainsFunc(token, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return "", true, fmt.Errorf("%s contains whitespace or control characters", KeyGitHubToken)
	}

	return token, true, nil
}

// WithoutGitHubToken returns environ without GITHUB_TOKEN, for child
// processes of the publisher that read untrusted input, such as the bundler:
// the token is meant for license detection only. The name is compared
// case-insensitively because Windows environment names are.
func WithoutGitHubToken(environ []string) []string {
	out := make([]string, 0, len(environ))

	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		if !strings.EqualFold(name, KeyGitHubToken) {
			out = append(out, entry)
		}
	}

	return out
}
