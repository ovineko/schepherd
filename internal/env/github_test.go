package env

import (
	"slices"
	"strings"
	"testing"
)

func TestGitHubToken(t *testing.T) {
	cases := []struct {
		value   *string
		name    string
		want    string
		wantSet bool
		wantErr bool
	}{
		{name: "unset"},
		{name: "blank", value: new("  \t")},
		{name: "token", value: new("ghs_abc123"), want: "ghs_abc123", wantSet: true},
		{name: "trailing newline", value: new("ghs_abc123\n"), want: "ghs_abc123", wantSet: true},
		{name: "inner space", value: new("ghs abc"), wantSet: true, wantErr: true},
		{name: "inner newline", value: new("ghs\nabc"), wantSet: true, wantErr: true},
		{name: "control character", value: new("ghs\x7fabc"), wantSet: true, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setOrUnset(t, KeyGitHubToken, tc.value)

			got, set, err := GitHubToken()
			if got != tc.want || set != tc.wantSet || (err != nil) != tc.wantErr {
				t.Fatalf("GitHubToken() = %q, %v, %v", got, set, err)
			}

			if err != nil && strings.Contains(err.Error(), "abc") {
				t.Fatalf("the error reveals the token: %v", err)
			}
		})
	}
}

func TestWithoutGitHubToken(t *testing.T) {
	environ := []string{"PATH=/usr/bin", "GITHUB_TOKEN=ghs_secret", "github_token=ghs_other", "GITHUB_TOKENS=kept", "LANG=C.UTF-8"}

	got := WithoutGitHubToken(environ)
	if want := []string{"PATH=/usr/bin", "GITHUB_TOKENS=kept", "LANG=C.UTF-8"}; !slices.Equal(got, want) {
		t.Fatalf("WithoutGitHubToken = %q, want %q", got, want)
	}

	if environ[1] != "GITHUB_TOKEN=ghs_secret" {
		t.Fatal("WithoutGitHubToken modified its input")
	}
}
