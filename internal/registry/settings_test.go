package registry

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote/errcode"
)

// repoRouter serves several single-repository fakes from one host.
type repoRouter map[string]*fakeRegistry

func (m repoRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	for repo, fake := range m {
		if strings.HasPrefix(r.URL.Path, "/v2/"+repo+"/") {
			fake.ServeHTTP(w, r)

			return
		}
	}

	writeRegistryError(w, http.StatusNotFound, errcode.ErrorCodeNameUnknown)
}

func TestSettingsLongestPrefixWins(t *testing.T) {
	client := newTestClient(map[string]HostConfig{
		"registry.example":           {CAFile: "host.pem"},
		"registry.example/org":       {CAFile: "org.pem"},
		"registry.example/org/team":  {CAFile: "team.pem"},
		"registry.example:5000":      {CAFile: "port.pem"},
		"registry.example/org/other": {CAFile: "other.pem"},
	})

	cases := []struct {
		repo    Repository
		wantKey string
		wantCA  string
	}{
		{Repository{Host: "registry.example", Path: "org"}, "registry.example/org", "org.pem"},
		{Repository{Host: "registry.example", Path: "org/schemas"}, "registry.example/org", "org.pem"},
		{Repository{Host: "registry.example", Path: "org/team/schemas"}, "registry.example/org/team", "team.pem"},
		{Repository{Host: "registry.example", Path: "org/teams/schemas"}, "registry.example/org", "org.pem"},
		{Repository{Host: "registry.example", Path: "organization/schemas"}, "registry.example", "host.pem"},
		{Repository{Host: "registry.example:5000", Path: "org/schemas"}, "registry.example:5000", "port.pem"},
		{Repository{Host: "other.example", Path: "org/schemas"}, "other.example", ""},
	}

	for _, tc := range cases {
		key, cfg := client.settings(tc.repo)
		if key != tc.wantKey || cfg.CAFile != tc.wantCA {
			t.Errorf("settings(%s) = %q %+v, want %q with ca_file %q", tc.repo, key, cfg, tc.wantKey, tc.wantCA)
		}
	}
}

func TestRepositoriesOnOneHostUseTheirOwnSettings(t *testing.T) {
	isolateDockerConfig(t)

	host := "registry.example"
	client := newTestClient(map[string]HostConfig{
		host:            {},
		host + "/org-a": {PlainHTTP: true},
		host + "/org-b": {PlainHTTP: true},
	})

	a1 := mustOpen(t, client, host, "org-a/schemas")
	a2 := mustOpen(t, client, host, "org-a/mirror")
	b := mustOpen(t, client, host, "org-b/schemas")
	other := mustOpen(t, client, host, "org-c/schemas")

	if a1.Target().Client != a2.Target().Client {
		t.Error("repositories under one entry do not share the authentication client")
	}

	if a1.Target().Client == b.Target().Client || a1.Target().Client == other.Target().Client || b.Target().Client == other.Target().Client {
		t.Error("repositories under different entries of one host share the authentication client")
	}

	if !a1.Target().PlainHTTP || !b.Target().PlainHTTP || other.Target().PlainHTTP {
		t.Errorf("plain_http = %v %v %v, want only the org-a and org-b entries to allow it",
			a1.Target().PlainHTTP, b.Target().PlainHTTP, other.Target().PlainHTTP)
	}
}

func TestRepositoriesOnOneHostUseTheirOwnCredentials(t *testing.T) {
	isolateDockerConfig(t)

	const (
		repoA = "org-a/schemas"
		repoB = "org-b/schemas"
	)

	fakeA, fakeB := newFakeRegistry(repoA), newFakeRegistry(repoB)
	fakeA.username, fakeA.password = "alice", "secret-a"
	fakeB.username, fakeB.password = "bob", "secret-b"
	descA := fakeA.addManifest(ocispec.MediaTypeImageManifest, testManifest(t))
	blob := []byte("copied from org-a to org-b")
	blobDesc := fakeA.addBlob(blob)

	srv := httptest.NewServer(repoRouter{repoA: fakeA, repoB: fakeB})
	t.Cleanup(srv.Close)
	host := hostOf(t, srv)

	dir := t.TempDir()
	credsA, credsB := filepath.Join(dir, "a.json"), filepath.Join(dir, "b.json")
	writeDockerConfig(t, credsA, map[string][2]string{host: {"alice", "secret-a"}})
	writeDockerConfig(t, credsB, map[string][2]string{host: {"bob", "secret-b"}})

	client := newTestClient(map[string]HostConfig{
		host:                  {PlainHTTP: true},
		host + "/org-a":       {PlainHTTP: true, CredentialsFile: credsA},
		host + "/" + repoB:    {PlainHTTP: true, CredentialsFile: credsB},
		host + "/org-b/other": {PlainHTTP: true, CredentialsFile: credsA},
	})
	ctx := context.Background()
	src, dst := mustOpen(t, client, host, repoA), mustOpen(t, client, host, repoB)

	for range 2 {
		if _, err := src.FetchManifestByDigest(ctx, descA.Digest.String(), ocispec.MediaTypeImageManifest, 0); err != nil {
			t.Fatalf("fetch from %s: %v", repoA, err)
		}

		var buf bytes.Buffer
		if err := src.FetchTo(ctx, blobDesc, &buf); err != nil || !bytes.Equal(buf.Bytes(), blob) {
			t.Fatalf("fetch blob from %s = %q, %v", repoA, buf.Bytes(), err)
		}

		if _, err := dst.PushBlob(ctx, blobDesc, blob); err != nil {
			t.Fatalf("push to %s: %v", repoB, err)
		}
	}

	for fake, user := range map[*fakeRegistry]string{fakeA: "alice", fakeB: "bob"} {
		authorized := 0

		for _, req := range fake.recorded() {
			if req.Authorization == "" {
				continue
			}

			authorized++

			decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(req.Authorization, "Basic "))
			if got, _, _ := strings.Cut(string(decoded), ":"); err != nil || got != user {
				t.Errorf("%s received Authorization %q (user %q), want the credentials of %q", fake.repo, req.Authorization, got, user)
			}
		}

		if authorized == 0 {
			t.Errorf("%s never received credentials", fake.repo)
		}
	}

	fakeB.mu.Lock()
	_, pushed := fakeB.blobs[blobDesc.Digest.String()]
	fakeB.mu.Unlock()

	if !pushed {
		t.Error("the blob did not reach org-b")
	}
}
