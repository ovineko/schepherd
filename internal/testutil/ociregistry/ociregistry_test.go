package ociregistry

import (
	"bytes"
	"io"
	"net/http"
	"testing"
)

func do(t *testing.T, method, url, contentType string, body []byte) (int, http.Header, []byte) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	return resp.StatusCode, resp.Header, data
}

func TestUploadsAreCheckedAgainstTheirDigest(t *testing.T) {
	r := New(t)
	base := "http://" + r.Host() + "/v2/org/repo"
	blob := []byte("blob")
	good := digestOf(blob)
	wrong := digestOf([]byte("other"))

	if code, _, _ := do(t, http.MethodPost, base+"/blobs/uploads/?digest="+wrong, "", blob); code != http.StatusBadRequest {
		t.Errorf("upload with a wrong digest: status %d", code)
	}

	code, header, _ := do(t, http.MethodPost, base+"/blobs/uploads/", "", nil)
	if code != http.StatusAccepted {
		t.Fatalf("start upload: status %d", code)
	}

	location := "http://" + r.Host() + header.Get("Location")
	if code, _, _ := do(t, http.MethodPut, location+"?digest="+good, "", blob); code != http.StatusCreated {
		t.Fatalf("finish upload: status %d", code)
	}

	if code, _, _ := do(t, http.MethodPut, location+"?digest="+good, "", blob); code != http.StatusNotFound {
		t.Errorf("a finished upload was accepted twice: status %d", code)
	}

	if code, _, body := do(t, http.MethodGet, base+"/blobs/"+good, "", nil); code != http.StatusOK || !bytes.Equal(body, blob) {
		t.Errorf("get blob: status %d, body %q", code, body)
	}

	manifest := []byte(`{"schemaVersion":2}`)
	if code, _, _ := do(t, http.MethodPut, base+"/manifests/"+wrong, "application/vnd.oci.image.manifest.v1+json", manifest); code != http.StatusBadRequest {
		t.Errorf("manifest under a wrong digest: status %d", code)
	}

	if code, _, _ := do(t, http.MethodPut, base+"/manifests/latest", "application/vnd.oci.image.manifest.v1+json", manifest); code != http.StatusCreated {
		t.Fatalf("tag manifest: status %d", code)
	}

	code, header, _ = do(t, http.MethodHead, base+"/manifests/latest", "", nil)
	if code != http.StatusOK || header.Get("Docker-Content-Digest") != digestOf(manifest) || header.Get("Content-Type") != "application/vnd.oci.image.manifest.v1+json" {
		t.Errorf("resolve tag: status %d, headers %v", code, header)
	}

	if code, _, body := do(t, http.MethodGet, base+"/tags/list", "", nil); code != http.StatusOK || string(body) != `{"name":"org/repo","tags":["latest"]}` {
		t.Errorf("tags: status %d, body %s", code, body)
	}

	if code, _, _ := do(t, http.MethodGet, "http://"+r.Host()+"/v2/org/other/tags/list", "", nil); code != http.StatusNotFound {
		t.Errorf("tags of an unknown repository: status %d", code)
	}

	if got := r.Requests(); got != 10 {
		t.Errorf("Requests() = %d, want 10", got)
	}

	r.Close()
	r.Close()
}
