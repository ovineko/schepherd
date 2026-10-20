// Package metaschema identifies immutable standard dialects embedded in validators.
package metaschema

import (
	"net/url"
	"slices"
	"strings"
)

var standardPaths = []string{
	"/draft-04/schema",
	"/draft-06/schema",
	"/draft-07/schema",
	"/draft/2019-09/schema",
	"/draft/2020-12/schema",
}

// IsStandard accepts only canonical standard dialect documents, not custom
// metaschemas, vocabulary documents, fragments or look-alike hosts.
func IsStandard(uri string) bool {
	u, err := url.Parse(uri)
	if err != nil {
		return false
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Opaque != "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || !strings.EqualFold(u.Host, "json-schema.org") {
		return false
	}
	return slices.Contains(standardPaths, u.EscapedPath())
}
