// Package cli implements the schepherd command line. Standard output carries
// only command results; diagnostics go to standard error.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/ovineko/schepherd/internal/artifact"
	"github.com/ovineko/schepherd/internal/buildinfo"
	"github.com/ovineko/schepherd/internal/cache"
	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/config"
	"github.com/ovineko/schepherd/internal/env"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/registry"
	"github.com/ovineko/schepherd/internal/store"
)

type globalFlags struct {
	config     string
	repository string
	catalog    string
	cacheDir   string
	workspace  string
	timeout    time.Duration
	offline    bool
	quiet      bool
}

type app struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	root   *cobra.Command
	cfg    *config.Config
	flags  globalFlags
}

// Main runs the command line and returns the process exit code.
func Main(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	a := &app{stdin: stdin, stdout: stdout, stderr: stderr}
	a.root = a.newRoot() //nolint:contextcheck // commands receive ctx through ExecuteContext and read it with cmd.Context()
	a.root.SetArgs(args)
	a.root.SetIn(stdin)
	a.root.SetOut(stdout)
	a.root.SetErr(stderr)

	err := a.root.ExecuteContext(ctx)
	if err == nil {
		return 0
	}

	if _, classified := errors.AsType[*fault.Error](err); !classified {
		if _, consumer := errors.AsType[*fault.ConsumerExitError](err); !consumer {
			err = fault.Wrap(fault.Usage, err, "invalid command line")
		}
	}

	a.report(err)

	return fault.ExitCodeOf(err)
}

func (a *app) report(err error) {
	if consumer, ok := errors.AsType[*fault.ConsumerExitError](err); ok {
		if !a.flags.quiet {
			_, _ = fmt.Fprintf(a.stderr, "schepherd: consumer exited with status %d\n", consumer.Code)
		}

		return
	}

	kind := fault.KindOf(err)
	_, _ = fmt.Fprintf(a.stderr, "schepherd: %s error: %v\n", kind, err)

	if kind == fault.Usage {
		_, _ = fmt.Fprintln(a.stderr, "Run 'schepherd --help' for usage.")
	}
}

func (a *app) logf(format string, args ...any) {
	if a.flags.quiet {
		return
	}

	_, _ = fmt.Fprintf(a.stderr, "schepherd: "+format+"\n", args...)
}

func (a *app) newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "schepherd",
		Short:         "Portable, reproducible JSON Schema distribution over OCI registries",
		Long:          "Schepherd fetches pinned JSON Schemas from OCI registries into verified local files,\nmirrors complete catalog snapshots, and runs your own validator on request.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.CompletionOptions.DisableDefaultCmd = true

	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return fault.Wrap(fault.Usage, err, "invalid flag")
	})

	pf := root.PersistentFlags()
	pf.StringVar(&a.flags.config, "config", "", "local configuration file (never discovered automatically; env "+env.KeyConfig+")")
	pf.StringVar(&a.flags.repository, "repository", "", "OCI repository host[:port]/path holding the schema set (env "+env.KeyRepository+")")
	pf.StringVar(&a.flags.catalog, "catalog", "", "sha256 digest of the catalog index (env "+env.KeyCatalog+")")
	pf.StringVar(&a.flags.cacheDir, "cache-dir", "", "cache directory (env "+env.KeyCacheDir+")")
	pf.BoolVar(&a.flags.offline, "offline", false, "forbid all network access by schepherd itself (env "+env.KeyOffline+")")
	pf.StringVar(&a.flags.workspace, "workspace", "", "workspace root for path matching (env "+env.KeyWorkspace+")")
	pf.DurationVar(&a.flags.timeout, "timeout", 0, "deadline for schepherd's own registry and cache work (default 10m; env "+env.KeyTimeout+")")
	pf.BoolVar(&a.flags.quiet, "quiet", false, "print only errors on stderr")

	root.AddCommand(
		a.newVersionCmd(),
		a.newPathCmd(),
		a.newCatCmd(),
		a.newExportCmd(),
		a.newCatalogCmd(),
		a.newListCmd(),
		a.newPatternsCmd(),
		a.newResolveCmd(),
		a.newPinCmd(),
		a.newMirrorCmd(),
		a.newRunCmd(),
		a.newConfigCmd(),
	)

	return root
}

func (a *app) flagOverrides() config.Overrides {
	var o config.Overrides

	flags := a.root.PersistentFlags()

	if flags.Changed("repository") {
		o.Repository = &a.flags.repository
	}

	if flags.Changed("catalog") {
		o.Catalog = &a.flags.catalog
	}

	if flags.Changed("cache-dir") {
		o.CacheDir = &a.flags.cacheDir
	}

	if flags.Changed("workspace") {
		o.Workspace = &a.flags.workspace
	}

	if flags.Changed("offline") {
		o.Offline = &a.flags.offline
	}

	if flags.Changed("timeout") {
		o.Timeout = &a.flags.timeout
	}

	return o
}

func envOverrides() (config.Overrides, error) {
	var o config.Overrides

	if v, ok := env.Repository(); ok {
		o.Repository = &v
	}

	if v, ok := env.Catalog(); ok {
		o.Catalog = &v
	}

	if v, ok := env.CacheDir(); ok {
		o.CacheDir = &v
	}

	if v, ok := env.Workspace(); ok {
		o.Workspace = &v
	}

	offline, set, err := env.Offline()
	if err != nil {
		return o, fault.Wrap(fault.Usage, err, "invalid environment")
	}

	if set {
		o.Offline = &offline
	}

	timeout, set, err := env.Timeout()
	if err != nil {
		return o, fault.Wrap(fault.Usage, err, "invalid environment")
	}

	if set {
		o.Timeout = &timeout
	}

	return o, nil
}

func (a *app) config() (*config.Config, error) {
	return a.loadConfig(false)
}

// transportConfig loads the configuration for commands that take their
// repositories as arguments: the [catalog] section and its overrides are not
// required, so pin can run before a catalog digest exists.
func (a *app) transportConfig() (*config.Config, error) {
	return a.loadConfig(true)
}

// loadConfig keeps its first result for the rest of the command, whichever
// mode asks later; every command uses only one mode.
func (a *app) loadConfig(withoutCatalog bool) (*config.Config, error) {
	if a.cfg != nil {
		return a.cfg, nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "determine current directory")
	}

	if a.root.PersistentFlags().Changed("timeout") && a.flags.timeout <= 0 {
		return nil, fault.New(fault.Usage, "--timeout must be positive")
	}

	envO, err := envOverrides()
	if err != nil {
		return nil, err
	}

	path := a.flags.config
	if !a.root.PersistentFlags().Changed("config") {
		path, _ = env.Config()
	}

	cfg, err := config.Load(config.LoadOptions{
		Path:           path,
		Cwd:            cwd,
		Lookup:         env.Lookup,
		Flags:          a.flagOverrides(),
		Env:            envO,
		WithoutCatalog: withoutCatalog,
	})
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "configuration")
	}

	a.cfg = cfg

	return cfg, nil
}

func artifactLimits(cfg *config.Config) artifact.Limits {
	limits := artifact.DefaultLimits()
	limits.MaxManifestBytes = cfg.Limits.MaxManifestBytes
	limits.MaxCatalogBytes = cfg.Limits.MaxCatalogBytes
	limits.MaxPayloadBytes = cfg.Limits.MaxPayloadBytes
	limits.MaxSchemaBytes = cfg.Limits.MaxSchemaBytes

	return limits
}

func catalogLimits(cfg *config.Config) catalog.Limits {
	return catalog.Limits{MaxEntries: cfg.Limits.MaxCatalogEntries, MaxManifestBytes: cfg.Limits.MaxManifestBytes}
}

func registryClient(cfg *config.Config) *registry.Client {
	hosts := make(map[string]registry.HostConfig, len(cfg.Registries))
	for host, r := range cfg.Registries {
		hosts[host] = registry.HostConfig{PlainHTTP: r.PlainHTTP, CAFile: r.CAFile, CredentialsFile: r.CredentialsFile}
	}

	return registry.NewClient(registry.Options{
		Hosts:            hosts,
		UserAgent:        "schepherd/" + buildinfo.Get().Version,
		Offline:          cfg.Offline,
		MaxManifestBytes: cfg.Limits.MaxManifestBytes,
	})
}

// cacheDirPath is the absolute cache directory; it touches nothing on disk.
func cacheDirPath(cfg *config.Config) (string, error) {
	dir := cfg.CacheDir
	if dir == "" {
		d, err := cache.DefaultDir()
		if err != nil {
			return "", fault.Wrap(fault.Usage, err, "no cache directory available; pass --cache-dir")
		}

		dir = d
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fault.Wrap(fault.Usage, err, "cache directory %q", dir)
	}

	return abs, nil
}

func (a *app) openCache(cfg *config.Config) (*cache.Cache, error) {
	dir, err := cacheDirPath(cfg)
	if err != nil {
		return nil, err
	}

	c, err := cache.Open(dir)
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "open cache %s", dir)
	}

	return c, nil
}

// session bundles what schema commands need: a verified catalog and a store.
type session struct {
	cfg     *config.Config
	cache   *cache.Cache
	store   *store.Store
	catalog *store.LoadedCatalog
	repo    registry.Repository
}

func (a *app) openSession(ctx context.Context) (*session, error) {
	cfg, err := a.config()
	if err != nil {
		return nil, err
	}

	if cfg.Repository == "" {
		return nil, fault.New(fault.Usage, "no repository configured; set catalog.repository, %s or --repository", env.KeyRepository)
	}

	if cfg.Catalog == "" {
		return nil, fault.New(fault.Usage, "no catalog digest configured; set catalog.digest, %s or --catalog (use 'schepherd pin' to resolve a tag once)", env.KeyCatalog)
	}

	repo, err := registry.ParseRepository(cfg.Repository)
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "catalog.repository")
	}

	c, err := a.openCache(cfg)
	if err != nil {
		return nil, err
	}

	client := registryClient(cfg)

	st := store.New(store.Options{
		Cache:          c,
		Repository:     repo.String(),
		Offline:        cfg.Offline,
		ArtifactLimits: artifactLimits(cfg),
		CatalogLimits:  catalogLimits(cfg),
		Log:            a.logf,
		Open: func() (store.Remote, error) {
			return client.Open(repo)
		},
	})

	loaded, err := st.Catalog(ctx, cfg.Catalog)
	if err != nil {
		_ = c.Close()

		return nil, fault.Wrap(fault.Internal, err, "load catalog")
	}

	return &session{cfg: cfg, cache: c, store: st, catalog: loaded, repo: repo}, nil
}

func (s *session) close() {
	_ = s.cache.Close()
}

func (s *session) entry(id string) (*catalog.Entry, error) {
	e, ok := s.catalog.Catalog.Lookup(id)
	if !ok {
		return nil, fault.New(fault.NotFound, "schema %q is not in catalog %s (revision %s); see 'schepherd list'", id, s.catalog.Digest, s.catalog.Catalog.Revision)
	}

	return e, nil
}

// withTimeout bounds schepherd's own work by --timeout.
func (a *app) withTimeout(ctx context.Context) (context.Context, context.CancelFunc, error) {
	cfg, err := a.config()
	if err != nil {
		return nil, nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)

	return ctx, cancel, nil
}
