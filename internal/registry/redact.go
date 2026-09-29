package registry

import (
	"net/url"
	"strconv"
	"strings"

	"oras.land/oras-go/v2/registry/remote/errcode"
)

// Registries answer blob and manifest downloads with a redirect to a storage
// URL whose query carries a time-limited signature (S3 presigning, Azure SAS,
// GCS signed URLs), and upload sessions keep state tokens in the query. The
// texts of net/http and ORAS errors quote the full URL of the failing hop, so
// every error this package returns shows URLs with scheme, host and path only.

// redactedError has the text of err with the user information, query and
// fragment of every URL removed. It unwraps to err, so errors.Is and
// errors.As still reach the original cause.
type redactedError struct {
	err error
	msg string
}

func (e *redactedError) Error() string {
	return e.msg
}

func (e *redactedError) Unwrap() error {
	return e.err
}

// redacted returns nil when the text of err shows no URL user information,
// query or fragment.
func redacted(err error) *redactedError {
	text := err.Error()

	clean := redactText(text, urlsIn(err))
	if clean == text {
		return nil
	}

	return &redactedError{err: err, msg: clean}
}

// redactURL returns u without user information, query and fragment.
func redactURL(u *url.URL) string {
	clean := *u
	clean.User = nil
	clean.RawQuery = ""
	clean.ForceQuery = false
	clean.Fragment = ""
	clean.RawFragment = ""

	return clean.String()
}

// redactRawURL redacts a URL given as text, which may be relative, such as
// the Location value net/http reports when it refuses a redirect.
func redactRawURL(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return redactURL(u)
	}

	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		return raw[:i]
	}

	return raw
}

// urlsIn collects the URLs that typed errors anywhere in the tree of err
// carry: the request URL of a *url.Error and the response URL of an
// *errcode.ErrorResponse.
func urlsIn(err error) []string {
	var urls []string

	for pending := []error{err}; len(pending) > 0; {
		e := pending[len(pending)-1]
		pending = pending[:len(pending)-1]

		switch typed := e.(type) { //nolint:errorlint // walks every node of the tree itself
		case *url.Error:
			urls = append(urls, typed.URL)
		case *errcode.ErrorResponse:
			if typed.URL != nil {
				urls = append(urls, typed.URL.String())
			}
		}

		switch wrapper := e.(type) { //nolint:errorlint // walks every node of the tree itself
		case interface{ Unwrap() error }:
			if next := wrapper.Unwrap(); next != nil {
				pending = append(pending, next)
			}
		case interface{ Unwrap() []error }:
			for _, next := range wrapper.Unwrap() {
				if next != nil {
					pending = append(pending, next)
				}
			}
		}
	}

	return urls
}

// redactText replaces every known URL in text, quoted the way %q prints it
// or bare, with its redacted form, and then strips the user information,
// query and fragment of any other absolute URL, such as the ones ORAS quotes
// in untyped errors.
func redactText(text string, known []string) string {
	for _, raw := range known {
		clean := redactRawURL(raw)
		if clean == raw {
			continue
		}

		text = strings.ReplaceAll(text, strconv.Quote(raw), strconv.Quote(clean))
		text = strings.ReplaceAll(text, raw, clean)
	}

	return stripQueries(text)
}

// stripQueries removes the user information, query and fragment of every
// absolute URL in text. A URL right after a double quote runs to the closing
// quote, skipping backslash escapes; any other URL runs to the next white
// space or quote.
func stripQueries(text string) string {
	var b strings.Builder

	for rest := text; ; {
		sep := strings.Index(rest, "://")
		if sep < 0 {
			b.WriteString(rest)

			return b.String()
		}

		start := sep
		for start > 0 && isSchemeByte(rest[start-1]) {
			start--
		}

		body := sep + len("://")
		quoted := start > 0 && rest[start-1] == '"'
		end := urlEnd(rest, body, quoted)

		b.WriteString(rest[:body])
		b.WriteString(stripURLParts(rest[body:end], quoted))

		rest = rest[end:]
	}
}

// stripURLParts drops the query, the fragment and the user information from
// what follows the "://" of a URL. Inside quotes, an escaped character never
// starts the query, so the cut cannot leave a backslash that escapes the
// closing quote.
func stripURLParts(u string, quoted bool) string {
	for i := 0; i < len(u); i++ {
		if quoted && u[i] == '\\' {
			i++

			continue
		}

		if u[i] == '?' || u[i] == '#' {
			u = u[:i]

			break
		}
	}

	authority := u
	if slash := strings.IndexByte(u, '/'); slash >= 0 {
		authority = u[:slash]
	}

	if at := strings.LastIndexByte(authority, '@'); at >= 0 {
		u = u[at+1:]
	}

	return u
}

func urlEnd(s string, from int, quoted bool) int {
	for i := from; i < len(s); i++ {
		switch c := s[i]; {
		case quoted && c == '\\':
			i++
		case c == '"':
			return i
		case !quoted && (c == ' ' || c == '\t' || c == '\n' || c == '\r'):
			return i
		}
	}

	return len(s)
}

func isSchemeByte(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '+' || c == '-' || c == '.'
}
