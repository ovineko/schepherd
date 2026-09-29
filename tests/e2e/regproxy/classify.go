package regproxy

import (
	"net/http"
	"regexp"
)

// Request classes of the OCI distribution API, as recorded in Record.Class.
const (
	ClassPing           = "ping"
	ClassManifestGet    = "manifest-get"
	ClassManifestHead   = "manifest-head"
	ClassManifestPut    = "manifest-put"
	ClassManifestDelete = "manifest-delete"
	ClassBlobGet        = "blob-get"
	ClassBlobHead       = "blob-head"
	ClassBlobUpload     = "blob-upload"
	ClassTags           = "tags"
	ClassReferrers      = "referrers"
	ClassCatalog        = "catalog"
	ClassOther          = "other"
)

// Repository names may contain slashes, so every pattern anchors on the
// route suffix and lets the name absorb the rest.
var (
	manifestRoute  = regexp.MustCompile(`^/v2/.+/manifests/[^/]+$`)
	uploadRoute    = regexp.MustCompile(`^/v2/.+/blobs/uploads(/[^/]*)?$`)
	blobRoute      = regexp.MustCompile(`^/v2/.+/blobs/[^/]+$`)
	tagsRoute      = regexp.MustCompile(`^/v2/.+/tags/list$`)
	referrersRoute = regexp.MustCompile(`^/v2/.+/referrers/[^/]+$`)
)

func classify(method, path string) string {
	switch {
	case path == "/v2/" || path == "/v2":
		if method == http.MethodGet || method == http.MethodHead {
			return ClassPing
		}
	case path == "/v2/_catalog":
		return ClassCatalog
	case uploadRoute.MatchString(path):
		if method == http.MethodPost || method == http.MethodPatch || method == http.MethodPut {
			return ClassBlobUpload
		}
	case manifestRoute.MatchString(path):
		return byMethod(method, ClassManifestGet, ClassManifestHead, ClassManifestPut, ClassManifestDelete)
	case blobRoute.MatchString(path):
		return byMethod(method, ClassBlobGet, ClassBlobHead, ClassOther, ClassOther)
	case tagsRoute.MatchString(path):
		return ClassTags
	case referrersRoute.MatchString(path):
		return ClassReferrers
	}

	return ClassOther
}

func byMethod(method, get, head, put, del string) string {
	switch method {
	case http.MethodGet:
		return get
	case http.MethodHead:
		return head
	case http.MethodPut:
		return put
	case http.MethodDelete:
		return del
	default:
		return ClassOther
	}
}
