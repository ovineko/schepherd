package registry

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/ovineko/schepherd/internal/fault"
)

func TestOpenOffline(t *testing.T) {
	client := NewClient(Options{
		Offline: true,
		Hosts: map[string]HostConfig{
			"ghcr.io": {CAFile: filepath.Join(t.TempDir(), "missing.pem"), CredentialsFile: filepath.Join(t.TempDir(), "missing.json")},
		},
	})

	repo, err := client.Open(Repository{Host: "ghcr.io", Path: "org/schemas"})
	if repo != nil {
		t.Error("Open returned a repository while offline")
	}

	requireKind(t, err, fault.Offline)

	if got, want := err.Error(), "network access is disabled (--offline)"; got != want {
		t.Errorf("error = %q, want %q", got, want)
	}

	if len(client.hosts) != 0 {
		t.Errorf("offline Open built %d host clients", len(client.hosts))
	}
}

func TestOpenRejectsInvalidRepository(t *testing.T) {
	client := newTestClient(nil)

	for _, repo := range []Repository{{}, {Host: "ghcr.io"}, {Host: "ghcr.io", Path: "Org/x"}, {Host: "ghcr.io", Path: "x:1"}} {
		_, err := client.Open(repo)
		requireKind(t, err, fault.Usage)
	}
}

func TestPlainHTTPIsPerHost(t *testing.T) {
	isolateDockerConfig(t)

	fake := newFakeRegistry(testRepoPath)
	desc := fake.addManifest(ocispec.MediaTypeImageManifest, testManifest(t))
	srv := fake.start(t)
	host := hostOf(t, srv)
	other := newFakeRegistry(testRepoPath)
	otherSrv := other.start(t)

	client := newTestClient(map[string]HostConfig{hostOf(t, otherSrv): {PlainHTTP: true}})

	_, err := mustOpen(t, client, host, testRepoPath).FetchManifestByDigest(context.Background(), desc.Digest.String(), ocispec.MediaTypeImageManifest, 0)
	requireKind(t, err, fault.Registry)

	if got := len(fake.recorded()); got != 0 {
		t.Fatalf("HTTPS attempt against a plain HTTP registry reached the handler %d times", got)
	}

	client = newTestClient(plainHosts(t, srv))

	if _, err := mustOpen(t, client, host, testRepoPath).FetchManifestByDigest(context.Background(), desc.Digest.String(), ocispec.MediaTypeImageManifest, 0); err != nil {
		t.Fatalf("plain HTTP fetch: %v", err)
	}
}

func TestCAFile(t *testing.T) {
	isolateDockerConfig(t)

	fake := newFakeRegistry(testRepoPath)
	desc := fake.addManifest(ocispec.MediaTypeImageManifest, testManifest(t))
	srv := fake.startTLS(t)
	host := hostOf(t, srv)
	ctx := context.Background()

	_, err := mustOpen(t, newTestClient(nil), host, testRepoPath).FetchManifestByDigest(ctx, desc.Digest.String(), ocispec.MediaTypeImageManifest, 0)
	requireKind(t, err, fault.Registry)
	requireMessage(t, err, "TLS verification of registry "+host+" failed", "ca_file")

	client := newTestClient(map[string]HostConfig{host: {CAFile: writeCAFile(t, srv)}})
	if _, err := mustOpen(t, client, host, testRepoPath).FetchManifestByDigest(ctx, desc.Digest.String(), ocispec.MediaTypeImageManifest, 0); err != nil {
		t.Fatalf("fetch with ca_file: %v", err)
	}

	notPEM := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(notPEM, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{notPEM, filepath.Join(t.TempDir(), "missing.pem")} {
		_, err := newTestClient(map[string]HostConfig{host: {CAFile: path}}).Open(Repository{Host: host, Path: testRepoPath})
		requireKind(t, err, fault.Usage)
		requireMessage(t, err, "ca_file", host)
	}
}

func TestRedirectFromHTTPSToHTTPIsRefused(t *testing.T) {
	isolateDockerConfig(t)

	mirror := &blobMirror{}
	mirrorSrv := httptest.NewServer(mirror)
	t.Cleanup(mirrorSrv.Close)

	fake := newFakeRegistry(testRepoPath)
	mirror.source = fake
	fake.redirectBlobsTo = mirrorSrv.URL
	desc := fake.addBlob([]byte("payload"))
	srv := fake.startTLS(t)
	host := hostOf(t, srv)

	client := newTestClient(map[string]HostConfig{host: {CAFile: writeCAFile(t, srv)}, hostOf(t, mirrorSrv): {PlainHTTP: true}})

	err := mustOpen(t, client, host, testRepoPath).FetchTo(context.Background(), desc, &bytes.Buffer{})
	requireKind(t, err, fault.Registry)
	requireMessage(t, err, "refusing redirect from HTTPS")

	if got := len(mirror.seen()); got != 0 {
		t.Errorf("downgraded redirect reached the plain HTTP server %d times", got)
	}
}

func TestBasicAuth(t *testing.T) {
	const decodedSecret = "ghp_SUPERSECRETTOKEN123"

	// A colon-less value is a common mistake; ORAS quotes the decoded value
	// in its error.
	malformed := func(t *testing.T, path, host string) {
		t.Helper()
		writeRawDockerConfig(t, path, `{"auths":{"`+host+`":{"auth":"`+base64.StdEncoding.EncodeToString([]byte(decodedSecret))+`"}}}`)
	}

	cases := []struct {
		setup     func(t *testing.T, host, dockerDir string) HostConfig
		name      string
		fragments []string
		wantKind  fault.Kind
		wantErr   bool
	}{
		{
			name:      "no credentials configured",
			setup:     func(*testing.T, string, string) HostConfig { return HostConfig{PlainHTTP: true} },
			wantErr:   true,
			wantKind:  fault.Registry,
			fragments: []string{"requires credentials but none are configured for it"},
		},
		{
			name: "wrong credentials from DOCKER_CONFIG",
			setup: func(t *testing.T, host, dockerDir string) HostConfig {
				t.Helper()
				writeDockerConfig(t, filepath.Join(dockerDir, "config.json"), map[string][2]string{host: {"user", "wrong"}})

				return HostConfig{PlainHTTP: true}
			},
			wantErr:   true,
			wantKind:  fault.Registry,
			fragments: []string{"authentication required or credentials rejected"},
		},
		{
			name: "credentials from DOCKER_CONFIG",
			setup: func(t *testing.T, host, dockerDir string) HostConfig {
				t.Helper()
				writeDockerConfig(t, filepath.Join(dockerDir, "config.json"), map[string][2]string{host: {"user", "secret"}})

				return HostConfig{PlainHTTP: true}
			},
		},
		{
			name: "credentials from credentials_file",
			setup: func(t *testing.T, host, _ string) HostConfig {
				t.Helper()

				path := filepath.Join(t.TempDir(), "auth.json")
				writeDockerConfig(t, path, map[string][2]string{host: {"user", "secret"}})

				return HostConfig{PlainHTTP: true, CredentialsFile: path}
			},
		},
		{
			name: "missing credentials_file",
			setup: func(t *testing.T, _, _ string) HostConfig {
				t.Helper()

				return HostConfig{PlainHTTP: true, CredentialsFile: filepath.Join(t.TempDir(), "absent.json")}
			},
			wantErr:   true,
			wantKind:  fault.Usage,
			fragments: []string{"credentials_file", "does not exist"},
		},
		{
			name: "malformed auth in credentials_file",
			setup: func(t *testing.T, host, _ string) HostConfig {
				t.Helper()

				path := filepath.Join(t.TempDir(), "auth.json")
				malformed(t, path, host)

				return HostConfig{PlainHTTP: true, CredentialsFile: path}
			},
			wantErr:   true,
			wantKind:  fault.Usage,
			fragments: []string{"credentials for registry", "in credentials_file", "auth.json", "are malformed"},
		},
		{
			name: "malformed auth in DOCKER_CONFIG",
			setup: func(t *testing.T, host, dockerDir string) HostConfig {
				t.Helper()
				malformed(t, filepath.Join(dockerDir, "config.json"), host)

				return HostConfig{PlainHTTP: true}
			},
			wantErr:   true,
			wantKind:  fault.Usage,
			fragments: []string{"in the Docker credentials configuration are malformed"},
		},
		{
			name: "credential helper that cannot run",
			setup: func(t *testing.T, host, dockerDir string) HostConfig {
				t.Helper()
				t.Setenv("PATH", t.TempDir())
				writeRawDockerConfig(t, filepath.Join(dockerDir, "config.json"), `{"credHelpers":{"`+host+`":"schepherd-test-absent"}}`)

				return HostConfig{PlainHTTP: true}
			},
			wantErr:   true,
			wantKind:  fault.Registry,
			fragments: []string{"cannot get the credentials for registry", `credential helper "docker-credential-schepherd-test-absent" cannot be run`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dockerDir := isolateDockerConfig(t)

			fake := newFakeRegistry(testRepoPath)
			fake.username, fake.password = "user", "secret"
			desc := fake.addManifest(ocispec.MediaTypeImageManifest, testManifest(t))
			srv := fake.start(t)
			host := hostOf(t, srv)

			client := newTestClient(map[string]HostConfig{host: tc.setup(t, host, dockerDir)})

			_, err := mustOpen(t, client, host, testRepoPath).FetchManifestByDigest(context.Background(), desc.Digest.String(), ocispec.MediaTypeImageManifest, 0)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("fetch: %v", err)
				}

				return
			}

			requireKind(t, err, tc.wantKind)
			requireMessage(t, err, tc.fragments...)
			requireMessage(t, err, host)

			for _, secret := range []string{"secret", "wrong", decodedSecret} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error leaks the credential %q: %v", secret, err)
				}
			}
		})
	}
}

func TestCredentialsStayOnTheirHost(t *testing.T) {
	cases := []struct {
		name      string
		prefix    string
		challenge bool
	}{
		{name: "one redirect"},
		{name: "one redirect to a challenging server", challenge: true},
		// net/http copies the first request's Authorization to every hop on
		// the same hostname whatever the port, so the second hop inside the
		// other origin is the one that would carry the credentials.
		{name: "two redirects inside the other origin", prefix: "/hop"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateDockerConfig(t)

			mirror := &blobMirror{challenge: tc.challenge}
			mirrorSrv := httptest.NewServer(mirror)
			t.Cleanup(mirrorSrv.Close)

			fake := newFakeRegistry(testRepoPath)
			fake.username, fake.password = "user", "secret"
			fake.redirectBlobsTo = mirrorSrv.URL + tc.prefix
			mirror.source = fake
			payload := []byte("payload served by the mirror")
			desc := fake.addBlob(payload)
			srv := fake.start(t)
			host, mirrorHost := hostOf(t, srv), hostOf(t, mirrorSrv)

			credentialsFile := filepath.Join(t.TempDir(), "auth.json")
			writeDockerConfig(t, credentialsFile, map[string][2]string{
				host:       {"user", "secret"},
				mirrorHost: {"mirror-user", "mirror-secret"},
			})

			client := newTestClient(map[string]HostConfig{
				host:       {PlainHTTP: true, CredentialsFile: credentialsFile},
				mirrorHost: {PlainHTTP: true, CredentialsFile: credentialsFile},
			})

			var buf bytes.Buffer

			err := mustOpen(t, client, host, testRepoPath).FetchTo(context.Background(), desc, &buf)
			if tc.challenge {
				requireKind(t, err, fault.Registry)
				requireMessage(t, err, "authentication required or credentials rejected")
			} else if err != nil || !bytes.Equal(buf.Bytes(), payload) {
				t.Fatalf("FetchTo through redirect = %q, %v", buf.Bytes(), err)
			}

			seen := mirror.seen()
			if want := 1 + strings.Count(tc.prefix, "/"); len(seen) != want {
				t.Fatalf("the mirror received %d requests, want %d", len(seen), want)
			}

			for i, header := range seen {
				if header != "" {
					t.Errorf("mirror request #%d carried Authorization %q", i+1, header)
				}
			}

			authorized := false

			for _, req := range fake.recorded() {
				authorized = authorized || req.Authorization == "Basic "+base64.StdEncoding.EncodeToString([]byte("user:secret"))
			}

			if !authorized {
				t.Error("the registry never received its own credentials")
			}
		})
	}
}

func TestCheckRedirectDropsAuthorizationOutsideTheFirstOrigin(t *testing.T) {
	cases := []struct {
		first, next string
		keep        bool
	}{
		{"https://registry.example/v2/a/blobs/x", "https://registry.example/v2/a/blobs/y", true},
		{"https://registry.example/v2/a/blobs/x", "https://REGISTRY.example:443/v2/a/blobs/y", true},
		{"http://registry.example:80/v2/a/blobs/x", "http://registry.example/v2/a/blobs/y", true},
		{"https://registry.example/v2/a/blobs/x", "https://blobs.registry.example/signed", false},
		{"https://registry.example/v2/a/blobs/x", "https://registry.example:8443/signed", false},
		{"http://registry.example/v2/a/blobs/x", "https://registry.example/v2/a/blobs/x", false},
		{"http://127.0.0.1:5000/v2/a/blobs/x", "http://127.0.0.1:5001/final", false},
	}

	for _, tc := range cases {
		first := httptest.NewRequest(http.MethodGet, tc.first, nil)
		hop := httptest.NewRequest(http.MethodGet, tc.first, nil)
		next := httptest.NewRequest(http.MethodGet, tc.next, nil)
		next.Header.Set("Authorization", "Basic c2VjcmV0")

		if err := checkRedirect(next, []*http.Request{first, hop}); err != nil {
			t.Fatalf("checkRedirect(%s -> %s): %v", tc.first, tc.next, err)
		}

		if kept := next.Header.Get("Authorization") != ""; kept != tc.keep {
			t.Errorf("redirect %s -> %s kept Authorization = %v, want %v", tc.first, tc.next, kept, tc.keep)
		}
	}
}

func TestCredentialFuncIgnoresOtherHosts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	writeDockerConfig(t, path, map[string][2]string{
		"registry.example": {"user", "secret"},
		"other.example":    {"other", "secret"},
	})

	credential := credentialFunc("registry.example", path)
	ctx := context.Background()

	got, err := credential(ctx, "other.example")
	if err != nil || got != auth.EmptyCredential {
		t.Errorf("credential for another host = %+v, %v; want none", got, err)
	}

	got, err = credential(ctx, "registry.example")
	if err != nil || got.Username != "user" || got.Password != "secret" {
		t.Errorf("credential for its own host = %+v, %v", got, err)
	}

	docker := credentialFunc("docker.io", path)
	if got, err := docker(ctx, "docker.io"); err != nil || got != auth.EmptyCredential {
		t.Errorf("docker.io credential requested as docker.io = %+v, %v; ORAS asks for registry-1.docker.io", got, err)
	}
}

func TestCredentialLookupErrorOmitsTheCause(t *testing.T) {
	const leaked = "hunter2"

	cases := []struct {
		err             error
		name            string
		credentialsFile string
		fragments       []string
		want            fault.Kind
		canceled        bool
	}{
		{
			name:      "malformed entry",
			err:       fmt.Errorf("failed to decode auth field: %w: auth '%s' does not conform", errors.New("invalid config format"), leaked),
			want:      fault.Usage,
			fragments: []string{"the credentials for registry registry.example in the Docker credentials configuration are malformed"},
		},
		{
			name:            "helper that cannot run",
			credentialsFile: "/etc/schepherd/auth.json",
			err:             &exec.Error{Name: "docker-credential-absent", Err: exec.ErrNotFound},
			want:            fault.Registry,
			fragments:       []string{`from credentials_file "/etc/schepherd/auth.json"`, `"docker-credential-absent" cannot be run: executable file not found`},
		},
		{
			name:      "helper failure output",
			err:       errors.New("error getting credentials: " + leaked),
			want:      fault.Registry,
			fragments: []string{"cannot get the credentials for registry registry.example from the Docker credentials configuration: the lookup failed"},
		},
		{name: "canceled", err: errors.New(leaked), canceled: true, want: fault.Canceled},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			if tc.canceled {
				cancel()
			} else {
				t.Cleanup(cancel)
			}

			err := Classify(credentialLookupError(ctx, "registry.example", tc.credentialsFile, tc.err), "fetch")
			requireKind(t, err, tc.want)
			requireMessage(t, err, tc.fragments...)

			if strings.Contains(err.Error(), leaked) {
				t.Errorf("error leaks the cause: %v", err)
			}
		})
	}
}

func TestCredentialsAreLazy(t *testing.T) {
	dockerDir := isolateDockerConfig(t)

	if err := os.WriteFile(filepath.Join(dockerDir, "config.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	invalid := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(invalid, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	open := newFakeRegistry(testRepoPath)
	openDesc := open.addManifest(ocispec.MediaTypeImageManifest, testManifest(t))
	openSrv := open.start(t)

	guarded := newFakeRegistry(testRepoPath)
	guarded.username, guarded.password = "user", "secret"
	guardedDesc := guarded.addManifest(ocispec.MediaTypeImageManifest, testManifest(t))
	guardedSrv := guarded.start(t)

	fromDocker := newFakeRegistry(testRepoPath)
	fromDockerDesc := fromDocker.addManifest(ocispec.MediaTypeImageManifest, testManifest(t))
	fromDockerSrv := fromDocker.start(t)

	client := newTestClient(map[string]HostConfig{
		hostOf(t, openSrv):       {PlainHTTP: true, CredentialsFile: invalid},
		hostOf(t, guardedSrv):    {PlainHTTP: true, CredentialsFile: invalid},
		hostOf(t, fromDockerSrv): {PlainHTTP: true},
	})
	ctx := context.Background()

	if _, err := mustOpen(t, client, hostOf(t, openSrv), testRepoPath).FetchManifestByDigest(ctx, openDesc.Digest.String(), ocispec.MediaTypeImageManifest, 0); err != nil {
		t.Fatalf("anonymous fetch read the invalid credentials_file: %v", err)
	}

	if _, err := mustOpen(t, client, hostOf(t, fromDockerSrv), testRepoPath).FetchManifestByDigest(ctx, fromDockerDesc.Digest.String(), ocispec.MediaTypeImageManifest, 0); err != nil {
		t.Fatalf("anonymous fetch read the invalid Docker config: %v", err)
	}

	_, err := mustOpen(t, client, hostOf(t, guardedSrv), testRepoPath).FetchManifestByDigest(ctx, guardedDesc.Digest.String(), ocispec.MediaTypeImageManifest, 0)
	requireKind(t, err, fault.Usage)
	requireMessage(t, err, "cannot load credentials_file", hostOf(t, guardedSrv))
}

func TestUserAgent(t *testing.T) {
	isolateDockerConfig(t)

	fake := newFakeRegistry(testRepoPath)
	desc := fake.addManifest(ocispec.MediaTypeImageManifest, testManifest(t))
	srv := fake.start(t)
	host := hostOf(t, srv)

	for _, tc := range []struct{ configured, wantPrefix string }{{"schepherd-test/1", "schepherd-test/1"}, {"", "schepherd/"}} {
		client := NewClient(Options{Hosts: plainHosts(t, srv), UserAgent: tc.configured, RetryPolicy: noRetry()})
		if _, err := mustOpen(t, client, host, testRepoPath).Exists(context.Background(), desc); err != nil {
			t.Fatal(err)
		}

		requests := fake.recorded()
		if got := requests[len(requests)-1].UserAgent; !strings.HasPrefix(got, tc.wantPrefix) {
			t.Errorf("User-Agent = %q, want prefix %q", got, tc.wantPrefix)
		}
	}
}

func TestRetryPolicy(t *testing.T) {
	isolateDockerConfig(t)

	fake := newFakeRegistry(testRepoPath)
	fake.setStatus(http.StatusServiceUnavailable)
	srv := fake.start(t)
	host := hostOf(t, srv)

	once := &retry.GenericPolicy{
		Retryable: retry.DefaultPredicate,
		Backoff:   func(int, *http.Response) time.Duration { return 0 },
		MaxRetry:  1,
	}

	for _, tc := range []struct {
		policy   retry.Policy
		name     string
		attempts int
	}{{noRetry(), "no retry", 1}, {once, "one retry", 2}} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(fake.recorded())
			client := NewClient(Options{Hosts: plainHosts(t, srv), RetryPolicy: tc.policy})

			_, err := mustOpen(t, client, host, testRepoPath).Resolve(context.Background(), "v1")
			requireKind(t, err, fault.Registry)
			requireMessage(t, err, "registry server error (503)")

			if got := len(fake.recorded()) - before; got != tc.attempts {
				t.Errorf("attempts = %d, want %d", got, tc.attempts)
			}
		})
	}

	if NewClient(Options{}).opts.RetryPolicy != retry.DefaultPolicy {
		t.Error("a nil RetryPolicy does not default to retry.DefaultPolicy")
	}

	if got := NewClient(Options{}).opts.MaxManifestBytes; got != DefaultMaxManifestBytes {
		t.Errorf("default MaxManifestBytes = %d", got)
	}
}

func TestHostClientsAreShared(t *testing.T) {
	isolateDockerConfig(t)

	client := newTestClient(nil)

	first := mustOpen(t, client, "registry.example", "a")
	second := mustOpen(t, client, "registry.example", "b")
	third := mustOpen(t, client, "other.example", "a")

	if first.Target().Client != second.Target().Client {
		t.Error("repositories on one host do not share the authentication client")
	}

	if first.Target().Client == third.Target().Client {
		t.Error("repositories on different hosts share the authentication client")
	}

	if first.Name() != (Repository{Host: "registry.example", Path: "a"}) {
		t.Errorf("Name = %+v", first.Name())
	}

	if first.Target().PlainHTTP {
		t.Error("PlainHTTP enabled without configuration")
	}
}
