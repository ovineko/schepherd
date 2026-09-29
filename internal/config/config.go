// Package config loads Schepherd's local TOML configuration.
//
// The configuration is a trusted, executable contract, so it is only ever read
// from a path the caller names; nothing is discovered. Local extends chains
// are merged file by file (tables by key, arrays replaced), every key is
// checked case-sensitively in every file, and relative paths stay relative to
// the file that declared them. ${NAME} references in the catalog, workspace,
// cache, registry and local schema paths are expanded at load time; runner
// templates stay raw because they are expanded per task at run time.
package config

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/env"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/interp"
	"github.com/ovineko/schepherd/internal/registry"
	"github.com/ovineko/schepherd/internal/runner"
)

// Version is the only supported config_version.
const Version = 1

// DefaultTimeout bounds Schepherd's own registry and cache work when neither
// --timeout nor SCHEPHERD_TIMEOUT is given. It has no configuration key.
const DefaultTimeout = 10 * time.Minute

const (
	maxLimitBytes   = 1 << 30
	maxLimitEntries = 1_000_000
)

var digestShapeRE = regexp.MustCompile(`^[a-z0-9]+(?:[.+_-][a-z0-9]+)*:[0-9A-Fa-f]{32,}$`)

// Limits bounds every read of registry content. Every limit is positive and
// at most 1 GiB (1,000,000 for MaxCatalogEntries); limits can be changed but
// never disabled.
type Limits struct {
	MaxManifestBytes  int64
	MaxCatalogBytes   int64
	MaxPayloadBytes   int64
	MaxSchemaBytes    int64
	MaxCatalogEntries int
}

// DefaultLimits returns the documented defaults.
func DefaultLimits() Limits {
	return Limits{
		MaxManifestBytes:  4 << 20,
		MaxCatalogBytes:   32 << 20,
		MaxPayloadBytes:   64 << 20,
		MaxSchemaBytes:    64 << 20,
		MaxCatalogEntries: 20000,
	}
}

// Registry holds the transport settings of one registry host or repository
// path prefix; Config.Registries is keyed by "host[:port]" or
// "host[:port]/path" as written. CAFile and CredentialsFile are absolute, or
// empty when not configured.
type Registry struct {
	CAFile          string
	CredentialsFile string
	PlainHTTP       bool
}

// Mapping is a local [[mappings]] rule; it takes precedence over catalog
// fileMatch rules.
type Mapping struct {
	Schema    string
	FileMatch []string
}

// LocalSchema is a [schemas."<id>"] entry: a JSON Schema file of the project
// itself, used in place instead of a catalog artifact. Path is absolute and
// clean; a relative value resolves against the directory of DeclaredIn, the
// file that set path, also when it is a base reached through extends. The
// file is only checked when a command uses it (see Read).
type LocalSchema struct {
	ID         string
	Path       string
	DeclaredIn string
	FileMatch  []string
	at         string
}

// Overrides are values given on the command line or through SCHEPHERD_*
// variables. A nil field was not given, so a given false or zero value still
// overrides the configuration file.
type Overrides struct {
	Repository *string
	Catalog    *string
	CacheDir   *string
	Workspace  *string
	Offline    *bool
	Timeout    *time.Duration
}

// LoadOptions selects the configuration file and the overrides applied on top
// of it.
type LoadOptions struct {
	// Lookup resolves ${NAME} references; nil treats every variable as unset.
	Lookup interp.LookupFunc
	// Path is the configuration file; empty means defaults only. A relative
	// path is resolved against Cwd.
	Path string
	// Cwd must be absolute. Relative override paths resolve against it and it
	// is the default workspace.
	Cwd   string
	Flags Overrides
	Env   Overrides
	// WithoutCatalog leaves Repository, Catalog and LocalSchemas empty and
	// skips expanding and checking them from every source, for commands that
	// name their repositories as arguments. Literal values in the file are
	// still checked when it is parsed.
	WithoutCatalog bool
}

// Config is the effective configuration after merging extends, applying
// overrides and expanding ${NAME} references outside the runner.
type Config struct {
	Registries map[string]Registry
	// Files lists every file read, bases first, each once in the order in
	// which it was first applied.
	Files    []string
	Mappings []Mapping
	// LocalSchemas is sorted by ID.
	LocalSchemas []LocalSchema
	Workspace    string
	// CacheDir is empty when nothing configures it; the caller then uses the
	// platform default.
	CacheDir   string
	Repository string
	Catalog    string
	// Runner keeps its templates unexpanded. It carries the defaults even
	// when RunnerConfigured is false.
	Runner           runner.Spec
	Limits           Limits
	Timeout          time.Duration
	Offline          bool
	RunnerConfigured bool
}

type setting struct {
	flag string
	env  string
}

var (
	workspaceSetting  = setting{flag: "--workspace", env: env.KeyWorkspace}
	cacheDirSetting   = setting{flag: "--cache-dir", env: env.KeyCacheDir}
	repositorySetting = setting{flag: "--repository", env: env.KeyRepository}
	catalogSetting    = setting{flag: "--catalog", env: env.KeyCatalog}
	timeoutSetting    = setting{flag: "--timeout", env: env.KeyTimeout}
)

// Load reads and validates the configuration and applies the precedence
// flags > environment > configuration file > defaults. A field whose value
// comes from a flag or the environment is not expanded from the file, so an
// unset variable there is not an error. Every error is classified as
// fault.Usage and names the file, position and key where one applies.
func Load(opts LoadOptions) (*Config, error) {
	if !filepath.IsAbs(opts.Cwd) {
		return nil, fault.New(fault.Internal, "config: working directory %q is not absolute", opts.Cwd)
	}

	b := &builder{opts: opts, lookup: opts.Lookup, merged: newLayer(""), base: filepath.Clean(opts.Cwd)}
	if b.lookup == nil {
		b.lookup = unsetLookup
	}

	var files []string

	if opts.Path != "" {
		top := absPath(opts.Cwd, opts.Path)

		l := newLoader()
		if err := l.visit(top, 0, nil); err != nil {
			return nil, err
		}

		for _, ly := range l.order {
			b.merged.apply(ly)
		}

		files = l.files()
		b.top = top
		b.base = filepath.Dir(top)
	}

	return b.build(files)
}

type builder struct {
	lookup interp.LookupFunc
	merged *layer
	top    string
	base   string
	opts   LoadOptions
}

func (b *builder) build(files []string) (*Config, error) {
	cfg := &Config{
		Files:    files,
		Limits:   b.limits(),
		Mappings: b.merged.mappings.val,
		Offline:  choose(b.opts.Flags.Offline, b.opts.Env.Offline, b.merged.offline, false),
	}

	var err error

	if cfg.Workspace, err = b.path(workspaceSetting, b.opts.Flags.Workspace, b.opts.Env.Workspace, b.merged.workspace); err != nil {
		return nil, err
	}

	if cfg.Workspace == "" {
		cfg.Workspace = filepath.Clean(b.opts.Cwd)
	}

	if cfg.CacheDir, err = b.path(cacheDirSetting, b.opts.Flags.CacheDir, b.opts.Env.CacheDir, b.merged.cacheDir); err != nil {
		return nil, err
	}

	if cfg.Timeout, err = b.timeout(); err != nil {
		return nil, err
	}

	if !b.opts.WithoutCatalog {
		if cfg.Repository, err = b.text(repositorySetting, b.opts.Flags.Repository, b.opts.Env.Repository, b.merged.repository, checkRepository); err != nil {
			return nil, err
		}

		if cfg.Catalog, err = b.text(catalogSetting, b.opts.Flags.Catalog, b.opts.Env.Catalog, b.merged.digest, checkDigest); err != nil {
			return nil, err
		}

		if cfg.LocalSchemas, err = b.localSchemas(); err != nil {
			return nil, err
		}
	}

	if cfg.Registries, err = b.registries(); err != nil {
		return nil, err
	}

	if cfg.Runner, cfg.RunnerConfigured, err = b.runner(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func choose[T any](flag, envValue *T, file opt[T], fallback T) T {
	switch {
	case flag != nil:
		return *flag
	case envValue != nil:
		return *envValue
	case file.set:
		return file.val
	default:
		return fallback
	}
}

func (b *builder) expand(v opt[string]) (string, error) {
	tpl, err := interp.Parse(v.val)
	if err != nil {
		return "", fault.Wrap(fault.Usage, err, "%s", v.at)
	}

	out, err := tpl.Expand(b.lookup, nil)
	if err != nil {
		return "", fault.Wrap(fault.Usage, err, "%s", v.at)
	}

	return out, nil
}

func (b *builder) filePath(v opt[string]) (string, error) {
	if !v.set {
		return "", nil
	}

	p, err := b.expand(v)
	if err != nil {
		return "", err
	}

	if p == "" {
		return "", fault.New(fault.Usage, "%s: expands to an empty path", v.at)
	}

	return absPath(filepath.Dir(v.at.file), p), nil
}

func (b *builder) path(s setting, flag, envValue *string, file opt[string]) (string, error) {
	var value, source string

	switch {
	case flag != nil:
		value, source = *flag, s.flag
	case envValue != nil:
		value, source = *envValue, s.env
	default:
		return b.filePath(file)
	}

	if value == "" {
		return "", fault.New(fault.Usage, "%s must not be empty", source)
	}

	return absPath(b.opts.Cwd, value), nil
}

func (b *builder) text(s setting, flag, envValue *string, file opt[string], check func(string) error) (string, error) {
	var value, source string

	switch {
	case flag != nil:
		value, source = *flag, s.flag
	case envValue != nil:
		value, source = *envValue, s.env
	case file.set:
		expanded, err := b.expand(file)
		if err != nil {
			return "", err
		}

		value, source = expanded, file.at.String()
	default:
		return "", nil
	}

	if err := check(value); err != nil {
		return "", fault.Wrap(fault.Usage, err, "%s", source)
	}

	return value, nil
}

func (b *builder) localSchemas() ([]LocalSchema, error) {
	ids := slices.Sorted(maps.Keys(b.merged.schemas))
	out := make([]LocalSchema, 0, len(ids))

	for _, id := range ids {
		s := b.merged.schemas[id]
		if !s.path.set {
			return nil, fault.New(fault.Usage, "%s: required key is missing; no file of the extends chain sets it", s.decl)
		}

		p, err := b.expand(s.path)
		if err != nil {
			return nil, err
		}

		if p == "" {
			return nil, fault.New(fault.Usage, "%s: expands to an empty path", s.path.at)
		}

		if err := checkLocalPath(p); err != nil {
			return nil, fault.Wrap(fault.Usage, err, "%s", s.path.at)
		}

		out = append(out, LocalSchema{
			ID:         id,
			Path:       absPath(filepath.Dir(s.path.at.file), p),
			DeclaredIn: s.path.at.file,
			FileMatch:  slices.Clone(s.fileMatch.val),
			at:         s.path.at.String(),
		})
	}

	return out, nil
}

// LocalSchema returns the [schemas] entry with the given id.
func (c *Config) LocalSchema(id string) (*LocalSchema, bool) {
	i, found := slices.BinarySearchFunc(c.LocalSchemas, id, func(s LocalSchema, target string) int {
		return strings.Compare(s.ID, target)
	})
	if !found {
		return nil, false
	}

	return &c.LocalSchemas[i], true
}

func (b *builder) timeout() (time.Duration, error) {
	var (
		value  time.Duration
		source string
	)

	switch {
	case b.opts.Flags.Timeout != nil:
		value, source = *b.opts.Flags.Timeout, timeoutSetting.flag
	case b.opts.Env.Timeout != nil:
		value, source = *b.opts.Env.Timeout, timeoutSetting.env
	default:
		return DefaultTimeout, nil
	}

	if value <= 0 {
		return 0, fault.New(fault.Usage, "%s must be a positive duration, got %s", source, value)
	}

	return value, nil
}

func (b *builder) limits() Limits {
	out := DefaultLimits()
	l := &b.merged.limits

	for _, f := range []struct {
		dst *int64
		src opt[int64]
	}{
		{&out.MaxManifestBytes, l.manifest},
		{&out.MaxCatalogBytes, l.catalog},
		{&out.MaxPayloadBytes, l.payload},
		{&out.MaxSchemaBytes, l.schema},
	} {
		if f.src.set {
			*f.dst = f.src.val
		}
	}

	if l.entries.set {
		out.MaxCatalogEntries = int(l.entries.val)
	}

	return out
}

func (b *builder) registries() (map[string]Registry, error) {
	out := make(map[string]Registry, len(b.merged.registries))

	for _, host := range slices.Sorted(maps.Keys(b.merged.registries)) {
		reg := b.merged.registries[host]
		r := Registry{PlainHTTP: reg.plainHTTP.val}

		var err error

		if r.CAFile, err = b.filePath(reg.caFile); err != nil {
			return nil, err
		}

		if r.CredentialsFile, err = b.filePath(reg.credentialsFile); err != nil {
			return nil, err
		}

		out[host] = r
	}

	return out, nil
}

func (b *builder) runner() (runner.Spec, bool, error) {
	r := &b.merged.runner
	spec := runner.Spec{
		Env:          make(map[string]string, len(r.env)),
		Mode:         runner.ModeBatch,
		Command:      r.command.val,
		Cwd:          runner.DefaultCwd,
		CwdBase:      b.base,
		Args:         []string{},
		Timeout:      runner.DefaultTimeout,
		Jobs:         runner.DefaultJobs,
		MaxArgsBytes: runner.DefaultMaxArgsBytes(),
		FailFast:     r.failFast.val,
		InheritEnv:   true,
	}

	if r.mode.set {
		spec.Mode = runner.Mode(r.mode.val)
	}

	if r.args.set {
		spec.Args = slices.Clone(r.args.val)
	}

	if r.cwd.set {
		spec.Cwd = r.cwd.val
		spec.CwdBase = filepath.Dir(r.cwd.at.file)
	}

	if r.timeout.set {
		spec.Timeout = r.timeout.val
	}

	if r.jobs.set {
		spec.Jobs = int(r.jobs.val)
	}

	if r.maxArgsBytes.set {
		spec.MaxArgsBytes = int(r.maxArgsBytes.val)
	}

	if r.inheritEnv.set {
		spec.InheritEnv = r.inheritEnv.val
	}

	for name, v := range r.env {
		spec.Env[name] = v.val
	}

	if !r.command.set {
		return spec, false, nil
	}

	if err := runner.ValidateSpec(spec); err != nil {
		return runner.Spec{}, false, fault.Wrap(fault.Usage, err, "%s", b.top)
	}

	return spec, true, nil
}

// checkHost defers to the registry client, which decides what a host is,
// and only adds the lower-case spelling when that is the fix: keys are
// matched verbatim, so an upper-case host could never be used.
func checkHost(host string) error {
	err := registry.ValidateHost(host)
	if err == nil {
		return nil
	}

	if lower := strings.ToLower(host); lower != host && registry.ValidateHost(lower) == nil {
		return fmt.Errorf("registry hosts and repository paths must be lower case; write host %q as %q", host, lower)
	}

	return err //nolint:wrapcheck // the callers add the setting and the expected form
}

const pathComponentRule = "must be lowercase letters and digits separated by '.', '_', '__' or '-'"

// checkRepository accepts what registry.ParseRepository accepts and explains
// the common mistakes: a scheme, tag or digest, and an invalid component.
func checkRepository(value string) error {
	const form = "must be host[:port]/path without scheme, tag or digest"

	switch {
	case value == "":
		return errors.New("repository must not be empty")
	case strings.Contains(value, "://"):
		return fmt.Errorf("repository %q %s", value, form)
	case strings.Contains(value, "@"):
		return fmt.Errorf("repository %q must not contain a digest; set the catalog digest separately", value)
	}

	host, path, ok := strings.Cut(value, "/")
	hostErr := checkHost(host)

	switch {
	case !ok || path == "" || errors.Is(hostErr, registry.ErrInvalidHost):
		return fmt.Errorf("repository %q %s", value, form)
	case hostErr != nil:
		return fmt.Errorf("repository %q: %w", value, hostErr)
	}

	for segment := range strings.SplitSeq(path, "/") {
		if strings.Contains(segment, ":") {
			return fmt.Errorf("repository %q must not contain a tag; tags are resolved only by \"schepherd pin\"", value)
		}

		if registry.ValidatePath(segment) != nil {
			return fmt.Errorf("repository %q: path component %q %s", value, segment, pathComponentRule)
		}
	}

	if _, err := registry.ParseRepository(value); err != nil {
		return fmt.Errorf("repository %q %s: %w", value, form, err)
	}

	return nil
}

// checkRegistryKey accepts "host[:port]" for every repository on a host and
// "host[:port]/path" for the repositories at or below that path: exactly
// the hosts and repositories the registry client accepts.
func checkRegistryKey(key string) error {
	const form = "registry key must be host[:port] or host[:port]/path without scheme, tag or digest"

	host, path, hasPath := strings.Cut(key, "/")
	if err := checkHost(host); errors.Is(err, registry.ErrInvalidHost) {
		return errors.New(form)
	} else if err != nil {
		return err
	}

	if !hasPath {
		return nil
	}

	for segment := range strings.SplitSeq(path, "/") {
		if registry.ValidatePath(segment) != nil {
			return fmt.Errorf("%s; path component %q %s", form, segment, pathComponentRule)
		}
	}

	if _, err := registry.ParseRepository(key); err != nil {
		return fmt.Errorf("%s: %w", form, err)
	}

	return nil
}

// checkDigest accepts only a sha256 manifest digest. Tags are refused because
// a tag can move; "schepherd pin" resolves one explicitly, once.
func checkDigest(value string) error {
	err := digest.Validate(value)
	if err == nil {
		return nil
	}

	const hint = "run \"schepherd pin <repository>:<tag>\" once and configure the sha256 digest it prints"

	switch {
	case value == "":
		return fmt.Errorf("catalog digest must not be empty; %s", hint)
	case strings.Contains(value, "@"):
		return fmt.Errorf("%q is a full reference, not a digest; configure only the sha256:<hex> part after '@', or %s", value, hint)
	case !strings.HasPrefix(value, digest.Algorithm+":") && !digestShapeRE.MatchString(value):
		return fmt.Errorf("%q looks like a tag, not a digest; tags are never resolved implicitly: %s", value, hint)
	default:
		return fmt.Errorf("%w; %s", err, hint)
	}
}
