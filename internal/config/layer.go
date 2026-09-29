package config

import (
	"fmt"
	"maps"
	"time"
)

// origin records where a value was declared so that errors can name the file,
// position and key, and so that relative paths resolve against the directory
// of the declaring file rather than the file that extends it.
type origin struct {
	file string
	key  string
	line int
	col  int
}

func (o origin) String() string {
	switch {
	case o.line > 0 && o.key != "":
		return fmt.Sprintf("%s:%d:%d: %s", o.file, o.line, o.col, o.key)
	case o.key != "":
		return o.file + ": " + o.key
	default:
		return o.file
	}
}

type opt[T any] struct {
	val T
	at  origin
	set bool
}

func (o *opt[T]) override(src opt[T]) {
	if src.set {
		*o = src
	}
}

type extendsRef struct {
	path string
	at   origin
}

// layer is one configuration file after validation, or the merged result of
// several. Only keys present in the file are set.
type layer struct {
	registries map[string]*registryLayer
	schemas    map[string]*localSchemaLayer
	file       string
	extends    []extendsRef
	runner     runnerLayer
	workspace  opt[string]
	cacheDir   opt[string]
	repository opt[string]
	digest     opt[string]
	mappings   opt[[]Mapping]
	limits     limitsLayer
	offline    opt[bool]
}

type limitsLayer struct {
	manifest opt[int64]
	catalog  opt[int64]
	payload  opt[int64]
	schema   opt[int64]
	entries  opt[int64]
}

// localSchemaLayer is one [schemas."<id>"] table. Like every other table it
// merges key by key, so path may come from a base and file_match from the
// file that extends it; path is only required after merging, and a missing
// one is reported at decl, the last declaration of the id.
type localSchemaLayer struct {
	path      opt[string]
	fileMatch opt[[]string]
	decl      origin
}

type registryLayer struct {
	caFile          opt[string]
	credentialsFile opt[string]
	plainHTTP       opt[bool]
}

type runnerLayer struct {
	env          map[string]opt[string]
	mode         opt[string]
	command      opt[string]
	cwd          opt[string]
	args         opt[[]string]
	timeout      opt[time.Duration]
	jobs         opt[int64]
	maxArgsBytes opt[int64]
	failFast     opt[bool]
	inheritEnv   opt[bool]
}

func newLayer(file string) *layer {
	return &layer{
		file:       file,
		registries: map[string]*registryLayer{},
		schemas:    map[string]*localSchemaLayer{},
		runner:     runnerLayer{env: map[string]opt[string]{}},
	}
}

func (l *layer) apply(src *layer) {
	l.workspace.override(src.workspace)
	l.cacheDir.override(src.cacheDir)
	l.offline.override(src.offline)
	l.repository.override(src.repository)
	l.digest.override(src.digest)
	l.mappings.override(src.mappings)

	for id, s := range src.schemas {
		dst, ok := l.schemas[id]
		if !ok {
			dst = &localSchemaLayer{}
			l.schemas[id] = dst
		}

		dst.path.override(s.path)
		dst.fileMatch.override(s.fileMatch)
		dst.decl = s.decl
	}

	l.limits.manifest.override(src.limits.manifest)
	l.limits.catalog.override(src.limits.catalog)
	l.limits.payload.override(src.limits.payload)
	l.limits.schema.override(src.limits.schema)
	l.limits.entries.override(src.limits.entries)

	for host, reg := range src.registries {
		dst, ok := l.registries[host]
		if !ok {
			dst = &registryLayer{}
			l.registries[host] = dst
		}

		dst.plainHTTP.override(reg.plainHTTP)
		dst.caFile.override(reg.caFile)
		dst.credentialsFile.override(reg.credentialsFile)
	}

	r, s := &l.runner, &src.runner
	r.mode.override(s.mode)
	r.command.override(s.command)
	r.args.override(s.args)
	r.cwd.override(s.cwd)
	r.timeout.override(s.timeout)
	r.jobs.override(s.jobs)
	r.maxArgsBytes.override(s.maxArgsBytes)
	r.failFast.override(s.failFast)
	r.inheritEnv.override(s.inheritEnv)
	maps.Copy(r.env, s.env)
}
