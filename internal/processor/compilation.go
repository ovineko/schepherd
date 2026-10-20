package processor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/ovineko/schepherd/internal/cache"
	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/interp"
	"github.com/ovineko/schepherd/internal/jsonutil"
	"github.com/ovineko/schepherd/internal/metaschema"
	"github.com/ovineko/schepherd/internal/runner"
)

const compilationRecipe = "schepherd-processor-compilation-v3"

// Prepared carries a verified compiler artifact, never a document-validation verdict.
type Prepared struct {
	cache.Compilation

	Configuration string
}

// Compiler keeps compilation identities in memory and artifacts in the supplied cache.
// A nil cache retains artifacts only until Close; verify-only never compiles or writes.
type Compiler struct {
	Cache             *cache.Cache
	VerifyOnly        bool
	RequirePersistent bool
	identities        map[string]string
	config            string
	temp              string
}

// Close removes invocation-local compiler artifacts, not persistent cache entries.
func (c *Compiler) Close() {
	if c.temp != "" {
		_ = os.RemoveAll(c.temp)
	}
}

// Prepare reuses a compilation only when schema bytes, compiler and settings agree.
// Local schemas with external references remain invocation-local until their full
// dependency graph can be included in the recipe.
func (c *Compiler) Prepare(ctx context.Context, definition Definition, task runner.Task, schema string, data []byte, catalog bool) (Prepared, error) {
	if definition.Type == "custom" {
		return Prepared{}, nil
	}
	persistent := c.Cache != nil && cacheableSchema(data, catalog) && !InputDependent(definition)
	if !persistent && (c.VerifyOnly || c.RequirePersistent) {
		return Prepared{}, fault.New(fault.Usage, "schema %s has dependencies or settings that cannot be cached safely", schema)
	}
	configuration := ""
	if definition.Type == "sourcemeta" {
		var err error
		configuration, err = c.configuration(ctx)
		if err != nil {
			return Prepared{}, err
		}
	}
	if !persistent {
		return c.prepareTemporary(ctx, definition, task, schema, configuration)
	}
	return c.prepareCached(ctx, definition, task, schema, data, configuration)
}

func (c *Compiler) prepareTemporary(ctx context.Context, definition Definition, task runner.Task, schema, configuration string) (Prepared, error) {
	result, artifact, err := c.compile(ctx, definition, task, schema, configuration)
	if err != nil || len(artifact) == 0 {
		return Prepared{Compilation: result, Configuration: configuration}, err
	}
	result.Path, err = c.temporary(artifact)
	return Prepared{Compilation: result, Configuration: configuration}, err
}

func (c *Compiler) prepareCached(ctx context.Context, definition Definition, task runner.Task, schema string, data []byte, configuration string) (Prepared, error) {
	key, err := c.key(ctx, definition, task, data)
	if err != nil {
		return Prepared{}, err
	}
	result, err := c.load(key, definition.Type)
	if err == nil {
		return Prepared{Compilation: result, Configuration: configuration}, nil
	}
	if !errors.Is(err, cache.ErrNotFound) && !errors.Is(err, cache.ErrCorrupt) {
		return Prepared{}, fault.Wrap(fault.Internal, err, "load processor compilation")
	}
	if c.VerifyOnly {
		return Prepared{}, fault.Wrap(fault.Offline, err, "compiled processor %s is not prepared", definition.Spec.Name)
	}
	release, err := c.Cache.Lock(ctx, "compile-"+digest.Hex(key))
	if err != nil {
		return Prepared{}, fault.Wrap(fault.Internal, err, "lock processor compilation; prepare this cache before using a read-only copy")
	}
	defer release()
	result, err = c.load(key, definition.Type)
	if err == nil {
		return Prepared{Compilation: result, Configuration: configuration}, nil
	}
	if !errors.Is(err, cache.ErrNotFound) && !errors.Is(err, cache.ErrCorrupt) {
		return Prepared{}, fault.Wrap(fault.Internal, err, "recheck processor compilation")
	}
	snapshot, err := c.temporary(data)
	if err != nil {
		return Prepared{}, err
	}
	result, artifact, err := c.compile(ctx, definition, task, snapshot, configuration)
	if err != nil {
		return Prepared{}, err
	}
	if definition.Type == "sourcemeta" && artifact != nil {
		artifact, err = portableTemplate(artifact, snapshot, digest.FromBytes(data))
		if err != nil {
			return Prepared{}, err
		}
	}
	result.Reason = strings.ReplaceAll(result.Reason, snapshot, "{schema}")
	result.Reason = strings.ReplaceAll(result.Reason, schema, "{schema}")
	result.Reason = strings.ReplaceAll(result.Reason, task.Path, "{processor}")
	result, err = c.Cache.SaveCompilation(ctx, key, result.Reason, artifact)
	if err != nil {
		return Prepared{}, fault.Wrap(fault.Internal, err, "save processor compilation")
	}
	return Prepared{Compilation: result, Configuration: configuration}, nil
}

func (c *Compiler) load(key, kind string) (cache.Compilation, error) {
	result, err := c.Cache.LoadCompilation(key)
	if err == nil && result.Reason == "" && result.Path == "" && kind != "check-jsonschema" {
		return cache.Compilation{}, fault.New(fault.Integrity, "processor compilation %s has no required template", key)
	}
	if err != nil {
		return cache.Compilation{}, fmt.Errorf("read compilation record: %w", err)
	}
	return result, nil
}

// Apply uses the exact artifact returned by Prepare for this task's schema.
func (p Prepared) Apply(task *runner.Task) {
	if p.Configuration != "" {
		task.Args = append(task.Args, "--configuration", p.Configuration)
	}
	if p.Path == "" {
		return
	}
	flag := "--compiled"
	if task.Processor.Type == "sourcemeta" {
		flag = "--template"
	}
	task.Args = append(task.Args, flag, p.Path)
}

func (c *Compiler) configuration(ctx context.Context) (string, error) {
	if c.config != "" {
		return c.config, nil
	}
	if c.Cache == nil {
		var err error
		c.config, err = c.temporary([]byte("{}"))
		return c.config, err
	}
	key := digest.FromBytes([]byte(compilationRecipe + ":sourcemeta-configuration:{}"))
	entry, err := c.Cache.LoadCompilation(key)
	if err != nil && (errors.Is(err, cache.ErrNotFound) || errors.Is(err, cache.ErrCorrupt)) && !c.VerifyOnly {
		release, lockErr := c.Cache.Lock(ctx, "compile-"+digest.Hex(key))
		if lockErr != nil {
			return "", fault.Wrap(fault.Internal, lockErr, "lock compiler configuration")
		}
		defer release()
		entry, err = c.Cache.SaveCompilation(ctx, key, "", []byte("{}"))
	}
	if err != nil {
		return "", fault.Wrap(fault.Offline, err, "isolated compiler configuration is not prepared")
	}
	if entry.Path == "" || entry.Reason != "" {
		return "", fault.New(fault.Integrity, "invalid isolated compiler configuration")
	}
	c.config = entry.Path
	return c.config, nil
}

func (c *Compiler) temporary(data []byte) (string, error) {
	if c.temp == "" {
		var err error
		c.temp, err = os.MkdirTemp("", "schepherd-compiled-*")
		if err != nil {
			return "", fault.Wrap(fault.Internal, err, "create temporary compiler directory")
		}
	}
	file, err := os.CreateTemp(c.temp, "artifact-*.json")
	if err != nil {
		return "", fault.Wrap(fault.Internal, err, "create temporary compiler artifact")
	}
	_, writeErr := file.Write(data)
	err = errors.Join(writeErr, file.Close())
	if err != nil {
		return "", fault.Wrap(fault.Internal, err, "write temporary compiler artifact")
	}
	return file.Name(), nil
}

func (c *Compiler) key(ctx context.Context, definition Definition, task runner.Task, schema []byte) (string, error) {
	identityEnv := slices.Clone(task.Env)
	slices.Sort(identityEnv)
	identityKey := task.Path + "\x00" + definition.Type + "\x00" + strings.Join(identityEnv, "\x00")
	if c.identities == nil {
		c.identities = make(map[string]string)
	}
	identity, ok := c.identities[identityKey]
	if !ok {
		file, err := os.Open(task.Path)
		if err != nil {
			return "", fault.Wrap(fault.ConsumerStart, err, "open processor executable")
		}
		hash := digest.NewHash()
		_, hashErr := io.Copy(hash, file)
		if err := errors.Join(hashErr, file.Close()); err != nil {
			return "", fault.Wrap(fault.ConsumerStart, err, "hash processor executable")
		}
		identity = digest.FromHash(hash)
		if definition.Type == "ajv" || definition.Type == "check-jsonschema" {
			stdout, _, err := compilerCommand(ctx, task, []string{"identity"})
			if err != nil {
				return "", fault.Wrap(fault.ConsumerStart, err, "processor identity")
			}
			if err := jsonutil.Check(stdout, jsonutil.DefaultMaxDepth); err != nil {
				return "", fault.Wrap(fault.Integrity, err, "processor identity")
			}
			identity += ":" + digest.FromBytes(stdout)
		}
		c.identities[identityKey] = identity
	}
	settings := make([]string, 0, len(definition.Spec.Env))
	for _, entry := range task.Env {
		name, _, _ := strings.Cut(entry, "=")
		if _, explicit := definition.Spec.Env[name]; explicit || compilationEnvironment(definition.Type, name) {
			settings = append(settings, entry)
		}
	}
	slices.Sort(settings)
	recipe, err := json.Marshal(struct {
		Recipe     string   `json:"recipe"`
		Processor  string   `json:"processor"`
		Schema     string   `json:"schema"`
		Executable string   `json:"executable"`
		Platform   string   `json:"platform"`
		Settings   []string `json:"settings"`
	}{compilationRecipe, definition.Type, digest.FromBytes(schema), identity, runtime.GOOS + "/" + runtime.GOARCH, settings})
	if err != nil {
		return "", fault.Wrap(fault.Internal, err, "compilation recipe")
	}
	return digest.FromBytes(recipe), nil
}

func compilationEnvironment(kind, name string) bool {
	if strings.HasPrefix(name, "LD_") || strings.HasPrefix(name, "DYLD_") {
		return true
	}
	switch kind {
	case "ajv":
		return name == "NODE_OPTIONS" || name == "NODE_PATH"
	case "check-jsonschema":
		return strings.HasPrefix(name, "PYTHON") && name != "PYTHONUNBUFFERED" && name != "PYTHONDONTWRITEBYTECODE" && name != "PYTHONIOENCODING"
	default:
		return false
	}
}

func (c *Compiler) compile(ctx context.Context, definition Definition, task runner.Task, schema, configuration string) (cache.Compilation, []byte, error) {
	if definition.Type == "check-jsonschema" {
		reason, err := Probe(ctx, definition, task, schema)
		return cache.Compilation{Reason: reason}, nil, err
	}
	args := []string{"compile", schema}
	if definition.Type == "sourcemeta" {
		args = append(args, "--minify", "--configuration", configuration)
	}
	stdout, stderr, err := compilerCommand(ctx, task, args)
	if err != nil {
		if _, classified := errors.AsType[*fault.Error](err); classified {
			return cache.Compilation{}, nil, err
		}
		reason, classified := classifyProbeFailure(definition, err, stderr)
		return cache.Compilation{Reason: reason}, nil, classified
	}
	if definition.Type == "sourcemeta" {
		if err := jsonutil.Check(stdout, jsonutil.DefaultMaxDepth); err != nil {
			return cache.Compilation{}, nil, fault.Wrap(fault.Integrity, err, "compiled Sourcemeta template")
		}
		return cache.Compilation{}, stdout, nil
	}
	var result struct {
		Supported *bool   `json:"supported"`
		Reason    string  `json:"reason"`
		Template  *string `json:"template"`
	}
	if err := json.Unmarshal(stdout, &result); err != nil || result.Supported == nil {
		return cache.Compilation{}, nil, fault.New(fault.Integrity, "processor %s returned malformed compilation output", definition.Spec.Name)
	}
	if !*result.Supported {
		if result.Reason == "" {
			return cache.Compilation{}, nil, fault.New(fault.Integrity, "processor returned unsupported without a reason")
		}
		return cache.Compilation{Reason: result.Reason}, nil, nil
	}
	if result.Template == nil || *result.Template == "" {
		return cache.Compilation{}, nil, fault.New(fault.Integrity, "processor returned no compiled template")
	}
	return cache.Compilation{}, []byte(*result.Template), nil
}

func compilerCommand(ctx context.Context, task runner.Task, args []string) ([]byte, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	// bearer:disable go_gosec_injection_subproc_injection
	// The executable is explicitly bound by trusted processor configuration; no shell is involved.
	cmd := exec.CommandContext(ctx, task.Path, args...) //nolint:gosec // G204: explicitly configured compiler executable
	cmd.Dir, cmd.Env = task.Dir, task.Env
	cmd.WaitDelay = runner.DefaultKillGrace
	var stdout compiledOutput
	var stderr boundedOutput
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		kind := fault.ConsumerTimeout
		if errors.Is(ctx.Err(), context.Canceled) {
			kind = fault.Canceled
		}
		return nil, stderr.data, fault.Wrap(kind, ctx.Err(), "processor compilation")
	}
	if stdout.exceeded || stderr.exceeded {
		return nil, nil, fault.New(fault.Integrity, "processor compilation output exceeds limit")
	}
	if err != nil {
		return stdout.data, stderr.data, fmt.Errorf("execute processor compilation: %w", err)
	}
	return stdout.data, stderr.data, nil
}

type compiledOutput struct {
	data     []byte
	exceeded bool
}

func (w *compiledOutput) Write(data []byte) (int, error) {
	n := len(data)
	if len(data) > cache.MaxCompiledBytes-len(w.data) {
		w.exceeded = true
		data = data[:cache.MaxCompiledBytes-len(w.data)]
	}
	w.data = append(w.data, data...)
	return n, nil
}

func (w *compiledOutput) ReadFrom(r io.Reader) (int64, error) {
	var total int64
	for len(w.data) < cache.MaxCompiledBytes {
		if len(w.data) == cap(w.data) {
			capacity := min(cache.MaxCompiledBytes, max(32<<10, 2*cap(w.data)))
			data := make([]byte, len(w.data), capacity)
			copy(data, w.data)
			w.data = data
		}
		n, err := r.Read(w.data[len(w.data):min(cap(w.data), cache.MaxCompiledBytes)])
		w.data = w.data[:len(w.data)+n]
		total += int64(n)
		if errors.Is(err, io.EOF) {
			return total, nil
		}
		if err != nil {
			return total, fmt.Errorf("read processor compilation output: %w", err)
		}
	}
	n, err := io.Copy(io.Discard, r)
	w.exceeded = w.exceeded || n != 0
	if err != nil {
		return total + n, fmt.Errorf("drain processor compilation output: %w", err)
	}
	return total + n, nil
}

func cacheableSchema(data []byte, bundled bool) bool {
	var value any
	if json.Unmarshal(data, &value) != nil {
		return false
	}
	return noExternalDependencies(value, bundled)
}

func noExternalDependencies(value any, bundled bool) bool {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if key == "$schema" {
				if uri, ok := child.(string); ok && !metaschema.IsStandard(uri) {
					return false
				}
			}
			if !bundled && (key == "$ref" || key == "$dynamicRef" || key == "$recursiveRef") {
				if ref, ok := child.(string); ok && ref != "" && !strings.HasPrefix(ref, "#") {
					return false
				}
			}
			if !noExternalDependencies(child, bundled) {
				return false
			}
		}
	case []any:
		for _, child := range value {
			if !noExternalDependencies(child, bundled) {
				return false
			}
		}
	}
	return true
}

// InputDependent detects compilation settings that vary with each document.
func InputDependent(definition Definition) bool {
	for _, value := range definition.Spec.Env {
		if template, err := interp.Parse(value); err == nil && template.Uses(interp.File) {
			return true
		}
	}
	return false
}

func portableTemplate(artifact []byte, snapshot, schemaDigest string) ([]byte, error) {
	path := filepath.ToSlash(snapshot)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	base := (&url.URL{Scheme: "file", Path: path}).String()
	if !bytes.Contains(artifact, []byte(base)) {
		return artifact, nil
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(artifact))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "decode compiled template")
	}
	// Only the compiler knows this freshly generated snapshot URI. Replace its
	// diagnostic base, not identifiers or literal values from the original schema.
	value = portableLocations(value, base, "urn:schepherd:schema:"+digest.Hex(schemaDigest))
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "encode portable compiler template")
	}
	return data, nil
}

func portableLocations(value any, base, stable string) any {
	switch value := value.(type) {
	case string:
		if value == base || strings.HasPrefix(value, base+"#") {
			return stable + strings.TrimPrefix(value, base)
		}
	case []any:
		for index, child := range value {
			value[index] = portableLocations(child, base, stable)
		}
	case map[string]any:
		for key, child := range value {
			value[key] = portableLocations(child, base, stable)
		}
	}
	return value
}
