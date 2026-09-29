package registry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"

	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/ovineko/schepherd/internal/buildinfo"
	"github.com/ovineko/schepherd/internal/fault"
)

// DefaultMaxManifestBytes is the manifest size limit used when
// Options.MaxManifestBytes is not positive.
const DefaultMaxManifestBytes int64 = 4 << 20

const (
	maxRedirects = 10
	// maxTagPages bounds tag listing so a registry that keeps returning
	// "next" links cannot keep a command busy forever.
	maxTagPages = 10000
)

// HostConfig holds the settings for one registry "host[:port]" or for the
// repositories under one "host[:port]/path" prefix.
type HostConfig struct {
	// PlainHTTP allows unencrypted HTTP for exactly the repositories this
	// entry applies to. There is no automatic fallback from HTTPS to HTTP.
	PlainHTTP bool
	// CAFile is a PEM bundle trusted in addition to the system roots.
	CAFile string
	// CredentialsFile is a Docker-format config.json used instead of the
	// standard Docker configuration discovery.
	CredentialsFile string
}

// Options configures a Client.
type Options struct {
	// Hosts is keyed by "host[:port]" exactly as written in the repository,
	// or by "host[:port]/path" for that repository and every repository
	// below it. The entry with the longest matching key applies on its own;
	// nothing is inherited from a shorter key.
	Hosts     map[string]HostConfig
	UserAgent string
	// Offline makes every Open fail before any file is read or any
	// connection is made.
	Offline bool
	// MaxManifestBytes defaults to DefaultMaxManifestBytes.
	MaxManifestBytes int64
	// RetryPolicy defaults to retry.DefaultPolicy.
	RetryPolicy retry.Policy
}

// Client opens repositories and shares one HTTP and authentication client
// per Options.Hosts entry between them, so repositories on one host that use
// different entries never share credentials or cached tokens. It is safe for
// concurrent use.
type Client struct {
	hosts map[string]*auth.Client
	opts  Options
	mu    sync.Mutex
}

// NewClient returns a Client; it performs no I/O.
func NewClient(opts Options) *Client {
	if opts.MaxManifestBytes <= 0 {
		opts.MaxManifestBytes = DefaultMaxManifestBytes
	}

	if opts.RetryPolicy == nil {
		opts.RetryPolicy = retry.DefaultPolicy
	}

	if opts.UserAgent == "" {
		opts.UserAgent = "schepherd/" + buildinfo.Get().Version
	}

	opts.Hosts = maps.Clone(opts.Hosts)

	return &Client{opts: opts, hosts: map[string]*auth.Client{}}
}

// Open prepares access to repo. It fails with fault.Offline when the client is
// offline and with fault.Usage when the CA file of the applicable entry
// cannot be used. No request is sent and no credential is read until an
// operation needs it.
func (c *Client) Open(repo Repository) (*Repo, error) {
	if c.opts.Offline {
		return nil, fault.New(fault.Offline, "network access is disabled (--offline)")
	}

	parsed, err := parseRepository(repo.String())
	if err != nil || parsed != repo {
		return nil, fault.New(fault.Usage, "invalid repository %q", repo.String())
	}

	key, cfg := c.settings(repo)

	client, err := c.authClient(key, repo.Host, cfg)
	if err != nil {
		return nil, err
	}

	target := &remote.Repository{
		Client:           client,
		Reference:        registry.Reference{Registry: repo.Host, Repository: repo.Path},
		PlainHTTP:        cfg.PlainHTTP,
		MaxMetadataBytes: c.opts.MaxManifestBytes,
		TagListMaxPages:  maxTagPages,
	}

	return &Repo{name: repo, target: target, maxManifestBytes: c.opts.MaxManifestBytes}, nil
}

// settings returns the Options.Hosts entry with the longest key that applies
// to repo, or repo.Host and the zero configuration when none does.
func (c *Client) settings(repo Repository) (string, HostConfig) {
	best, cfg, found := repo.Host, HostConfig{}, false

	for key, hc := range c.opts.Hosts {
		if appliesTo(key, repo) && (!found || len(key) > len(best)) {
			best, cfg, found = key, hc, true
		}
	}

	return best, cfg
}

// appliesTo reports whether the settings key is repo's host or a path prefix
// of repo ending at a component boundary.
func appliesTo(key string, repo Repository) bool {
	if key == repo.Host {
		return true
	}

	prefix, ok := strings.CutPrefix(key, repo.Host+"/")
	if !ok || prefix == "" {
		return false
	}

	return repo.Path == prefix || strings.HasPrefix(repo.Path, prefix+"/")
}

func (c *Client) authClient(key, host string, cfg HostConfig) (*auth.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if client, ok := c.hosts[key]; ok {
		return client, nil
	}

	httpClient, err := newHTTPClient(key, cfg, c.opts.RetryPolicy)
	if err != nil {
		return nil, err
	}

	client := &auth.Client{
		Client:     httpClient,
		Header:     http.Header{"User-Agent": {c.opts.UserAgent}},
		Cache:      auth.NewCache(),
		Credential: credentialFunc(host, cfg.CredentialsFile),
	}
	c.hosts[key] = client

	return client, nil
}

// newHTTPClient names the registry in errors by its settings key, the way the
// configuration spells it.
func newHTTPClient(key string, cfg HostConfig, policy retry.Policy) (*http.Client, error) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fault.New(fault.Internal, "unexpected default HTTP transport %T", http.DefaultTransport)
	}

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}

	if cfg.CAFile != "" {
		pool, err := loadCAFile(key, cfg.CAFile)
		if err != nil {
			return nil, err
		}

		tlsConfig.RootCAs = pool
	}

	transport := base.Clone()
	transport.TLSClientConfig = tlsConfig

	retrying := retry.NewTransport(transport)
	retrying.Policy = func() retry.Policy { return policy }

	return &http.Client{Transport: retrying, CheckRedirect: checkRedirect}, nil
}

func loadCAFile(host, path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path is the ca_file the user configured for this host
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "cannot read ca_file for registry %s", host)
	}

	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}

	if !pool.AppendCertsFromPEM(data) {
		return nil, fault.New(fault.Usage, "ca_file %q for registry %s contains no PEM certificate", path, host)
	}

	return pool, nil
}

// checkRedirect keeps the standard redirect limit, refuses to follow a
// redirect from HTTPS to plain HTTP, which would be a silent downgrade, and
// drops Authorization from every hop outside the origin of the first request.
// The refusal names the target without its query, which may be a signature.
//
// The last rule closes a gap between net/http and ORAS v2.6.2: net/http copies
// the first request's Authorization to each hop whose hostname equals or is a
// subdomain of the first one, ignoring the port, while ORAS only strips it
// when a hop leaves the origin of the hop before. A second redirect inside a
// foreign origin would otherwise carry the registry credentials again. ORAS
// calls this function after net/http has copied the headers, so the deletion
// holds for the request that is actually sent.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}

	if via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
		return fmt.Errorf("refusing redirect from HTTPS to %s", redactURL(req.URL))
	}

	if origin(req.URL) != origin(via[0].URL) {
		req.Header.Del("Authorization")
	}

	return nil
}

// origin returns scheme, lower-case hostname and port, with the scheme's
// default port filled in, so "https://r.example" and "https://r.example:443"
// compare equal.
func origin(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)

	port := u.Port()
	if port == "" {
		switch scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}

	return scheme + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

// credentialFunc returns credentials only for host; any other hostport, such
// as a redirect target, gets none. The store is created on the first
// authentication challenge, so no credential file is read and no helper runs
// for registries that never answer 401.
func credentialFunc(host, credentialsFile string) auth.CredentialFunc {
	target := registry.Reference{Registry: host}.Host()
	store := sync.OnceValues(func() (credentials.Store, error) {
		return openCredentialStore(host, credentialsFile)
	})

	return func(ctx context.Context, hostport string) (auth.Credential, error) {
		if hostport != target {
			return auth.EmptyCredential, nil
		}

		s, err := store()
		if err != nil {
			return auth.EmptyCredential, err
		}

		cred, err := credentials.Credential(s)(ctx, hostport)
		if err != nil {
			return auth.EmptyCredential, credentialLookupError(ctx, host, credentialsFile, err)
		}

		return cred, nil
	}
}

// ORAS v2.6.2 keeps ErrInvalidConfigFormat in an internal package, so its
// text is the only way to tell a malformed entry from a failing helper.
// TestBasicAuth pins it.
const invalidConfigFormat = "invalid config format"

// credentialLookupError describes a failed lookup without the text of err:
// ORAS quotes the decoded auth value of a malformed entry, and a credential
// helper's failure output is not known to be free of secrets. Only the
// helper's name and the reason it could not be started are kept.
func credentialLookupError(ctx context.Context, host, credentialsFile string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("credential lookup for registry %s: %w", host, ctxErr)
	}

	source := "the Docker credentials configuration"
	if credentialsFile != "" {
		source = fmt.Sprintf("credentials_file %q", credentialsFile)
	}

	for cause := err; cause != nil; cause = errors.Unwrap(cause) {
		if cause.Error() == invalidConfigFormat {
			return fault.New(fault.Usage, "the credentials for registry %s in %s are malformed", host, source)
		}
	}

	if execErr, ok := errors.AsType[*exec.Error](err); ok {
		return fault.New(fault.Registry, "cannot get the credentials for registry %s from %s: credential helper %q cannot be run: %v",
			host, source, execErr.Name, execErr.Err)
	}

	return fault.New(fault.Registry, "cannot get the credentials for registry %s from %s: the lookup failed", host, source)
}

func openCredentialStore(host, credentialsFile string) (credentials.Store, error) {
	if credentialsFile == "" {
		store, err := credentials.NewStoreFromDocker(credentials.StoreOptions{})
		if err != nil {
			return nil, fault.Wrap(fault.Usage, err, "cannot load the Docker credentials configuration for registry %s", host)
		}

		return store, nil
	}

	if _, err := os.Stat(credentialsFile); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fault.New(fault.Usage, "credentials_file %q for registry %s does not exist", credentialsFile, host)
		}

		return nil, fault.Wrap(fault.Usage, err, "cannot read credentials_file for registry %s", host)
	}

	store, err := credentials.NewStore(credentialsFile, credentials.StoreOptions{})
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "cannot load credentials_file %q for registry %s", credentialsFile, host)
	}

	return store, nil
}
