package regproxy

import (
	"net/http"
	"testing"
)

func TestClassify(t *testing.T) {
	t.Parallel()

	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	tests := []struct {
		method string
		path   string
		want   string
	}{
		{http.MethodGet, "/v2/", ClassPing},
		{http.MethodHead, "/v2/", ClassPing},
		{http.MethodGet, "/v2", ClassPing},
		{http.MethodPost, "/v2/", ClassOther},
		{http.MethodGet, "/v2/_catalog", ClassCatalog},
		{http.MethodGet, "/v2/repo/manifests/latest", ClassManifestGet},
		{http.MethodGet, "/v2/org/team/schemas/manifests/" + digest, ClassManifestGet},
		{http.MethodHead, "/v2/repo/manifests/" + digest, ClassManifestHead},
		{http.MethodPut, "/v2/repo/manifests/catalog-20260923.1", ClassManifestPut},
		{http.MethodDelete, "/v2/repo/manifests/" + digest, ClassManifestDelete},
		{http.MethodPatch, "/v2/repo/manifests/latest", ClassOther},
		{http.MethodGet, "/v2/repo/blobs/" + digest, ClassBlobGet},
		{http.MethodGet, "/v2/org/manifests/blobs/" + digest, ClassBlobGet},
		{http.MethodHead, "/v2/repo/blobs/" + digest, ClassBlobHead},
		{http.MethodDelete, "/v2/repo/blobs/" + digest, ClassOther},
		{http.MethodPost, "/v2/repo/blobs/uploads/", ClassBlobUpload},
		{http.MethodPost, "/v2/org/repo/blobs/uploads", ClassBlobUpload},
		{http.MethodPatch, "/v2/repo/blobs/uploads/5f1c-uuid", ClassBlobUpload},
		{http.MethodPut, "/v2/repo/blobs/uploads/5f1c-uuid", ClassBlobUpload},
		{http.MethodGet, "/v2/repo/blobs/uploads/5f1c-uuid", ClassOther},
		{http.MethodDelete, "/v2/repo/blobs/uploads/5f1c-uuid", ClassOther},
		{http.MethodGet, "/v2/repo/tags/list", ClassTags},
		{http.MethodGet, "/v2/org/repo/tags/list", ClassTags},
		{http.MethodGet, "/v2/repo/referrers/" + digest, ClassReferrers},
		{http.MethodGet, "/v2/manifests/latest", ClassOther},
		{http.MethodGet, "/v2/repo/blobs/", ClassOther},
		{http.MethodGet, "/", ClassOther},
		{http.MethodGet, "/token", ClassOther},
		{http.MethodGet, "/v3/repo/manifests/latest", ClassOther},
	}

	for _, tc := range tests {
		if got := classify(tc.method, tc.path); got != tc.want {
			t.Errorf("classify(%s, %s) = %s, want %s", tc.method, tc.path, got, tc.want)
		}
	}
}
