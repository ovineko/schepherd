//go:build e2e

package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Scenarios download nothing. The preparation step (task test:e2e:prepare,
// which runs the test binary with E2E_PREPARE=1) pulls every pinned image,
// builds the validators image and caches the wheel build backend for uv;
// setUp refuses to start while an image is missing, and compose and
// dockerRun run with --pull never, so a scenario can neither fetch an image
// lazily nor hide a missing one behind a download.

const prepareHint = "run the preparation step first: pnpm dm exec task -- test:e2e:prepare " +
	"(or E2E_PREPARE=1 go test -tags e2e -count=1 -run '^$' -v ./tests/e2e)"

const (
	prepareTimeout = 30 * time.Minute
	pullTimeout    = 10 * time.Minute
)

// suiteImage is one image the scenarios run.
type suiteImage struct {
	Ref     string
	Purpose string
	// build creates a local image; nil means the image is pulled.
	build func(ctx context.Context) error
}

// suiteImages lists every image a run uses: the compose services of all
// profiles, the node and bun images of the npm packaging scenario, the
// Python and Ruby images of the PyPI and RubyGems scenario, and the locally
// built validators image.
func suiteImages(ctx context.Context, dir, jsonschema, artifacts string) ([]suiteImage, error) {
	refs, err := composeImages(ctx, filepath.Join(dir, "compose.yaml"))
	if err != nil {
		return nil, err
	}

	images := make([]suiteImage, 0, len(refs)+5)
	for _, ref := range refs {
		images = append(images, suiteImage{Ref: ref, Purpose: "compose service"})
	}

	images = append(images,
		suiteImage{Ref: nodeImage, Purpose: "npm packaging, E37"},
		suiteImage{Ref: bunImage, Purpose: "npm packaging, E37"},
		suiteImage{Ref: pythonImage, Purpose: "PyPI wheel, E41"},
		suiteImage{Ref: rubyImage, Purpose: "RubyGems package, E41"},
	)

	tag, err := validatorsTag(dir, jsonschema)
	if err != nil {
		return nil, err
	}

	images = append(images, suiteImage{
		Ref:     tag,
		Purpose: "validators and container builds, E10, E16, E31, E32, E38",
		build: func(ctx context.Context) error {
			return buildValidatorsImage(ctx, dir, jsonschema, artifacts, tag)
		},
	})

	return images, nil
}

// composeImages returns the distinct images of every service of compose.yaml
// in all profiles, as docker compose resolves them.
func composeImages(ctx context.Context, file string) ([]string, error) {
	// config interpolates the file but never reads the directories the
	// variables name.
	placeholders := []string{
		envComposeCertsDir + "=" + os.TempDir(),
		envComposeAuthDir + "=" + os.TempDir(),
		envComposeAuth2Dir + "=" + os.TempDir(),
	}

	out, err := runTool(ctx, filepath.Dir(file), hostEnviron(placeholders...), "docker",
		"compose", "--project-name", "schepherd-e2e-images", "--file", file,
		"--profile", profileAuth, "--profile", profileNpm, "config", "--images")
	if err != nil {
		return nil, fmt.Errorf("list the compose images: %w\n%s", err, out)
	}

	var refs []string

	for ref := range strings.FieldsSeq(string(out)) {
		if !slices.Contains(refs, ref) {
			refs = append(refs, ref)
		}
	}

	if len(refs) == 0 {
		return nil, fmt.Errorf("docker compose config --images listed no image for %s", file)
	}

	return refs, nil
}

func imagePresent(ctx context.Context, ref string) bool {
	_, err := docker(ctx, "image", "inspect", "--format", "{{.Id}}", ref)

	return err == nil
}

// missingImages returns the images that are not in the local image store.
func missingImages(ctx context.Context, images []suiteImage) []suiteImage {
	var missing []suiteImage

	for _, img := range images {
		if !imagePresent(ctx, img.Ref) {
			missing = append(missing, img)
		}
	}

	return missing
}

// checkImages fails when an image of the run is missing locally and names
// every missing one.
func checkImages(ctx context.Context, images []suiteImage) error {
	missing := missingImages(ctx, images)
	if len(missing) == 0 {
		return nil
	}

	lines := make([]string, 0, len(missing))
	for _, img := range missing {
		lines = append(lines, "  "+img.Ref+" ("+img.Purpose+")")
	}

	return fmt.Errorf("%d image(s) the scenarios use are not in the local image store, and scenarios never download anything:\n%s\n%s",
		len(missing), strings.Join(lines, "\n"), prepareHint)
}

// runPrepare is the preparation mode of the test binary: it pulls or builds
// every missing image and reports each one.
func runPrepare() int {
	ctx, cancel := context.WithTimeout(context.Background(), prepareTimeout)
	defer cancel()

	if err := prepare(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "e2e prepare: %v\n", err)

		return 1
	}

	return 0
}

func prepare(ctx context.Context) error {
	b, err := newBase(ctx)
	if err != nil {
		return err
	}

	images, err := suiteImages(ctx, b.dir, b.jsonschema, resolveArtifactsDir(b.root, "prepare"))
	if err != nil {
		return err
	}

	for _, img := range images {
		start := time.Now()

		switch {
		case imagePresent(ctx, img.Ref):
			fmt.Fprintf(os.Stderr, "e2e prepare: present  %s (%s)\n", img.Ref, img.Purpose)

			continue
		case img.build != nil:
			err = img.build(ctx)
		default:
			err = pullImage(ctx, img.Ref)
		}

		if err != nil {
			return err
		}

		verb := "pulled"
		if img.build != nil {
			verb = "built "
		}

		fmt.Fprintf(os.Stderr, "e2e prepare: %s   %s (%s) in %s\n", verb, img.Ref, img.Purpose, time.Since(start).Round(time.Second))
	}

	if err := checkImages(ctx, images); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "e2e prepare: all %d images are present; the scenarios will not download any\n", len(images))

	return cacheWheelBackend(ctx, b.root)
}

// cacheWheelBackend puts the hash-pinned build backend of the wheels into
// uv's cache, from which E41 builds them with UV_OFFLINE=1.
func cacheWheelBackend(ctx context.Context, root string) error {
	target, err := os.MkdirTemp("", "schepherd-e2e-wheel-backend-")
	if err != nil {
		return fmt.Errorf("create uv target: %w", err)
	}

	defer func() { _ = os.RemoveAll(target) }()

	out, err := runTool(ctx, root, hostEnviron(), "uv", "pip", "install", "--quiet", "--target", target, "--require-hashes",
		"--requirements", filepath.Join("packaging", "python", "build-constraints.txt"))
	if err != nil {
		return fmt.Errorf("cache the wheel build backend with uv: %w\n%s", err, out)
	}

	fmt.Fprintf(os.Stderr, "e2e prepare: cached the wheel build backend (packaging/python/build-constraints.txt) for uv\n")

	return nil
}

func pullImage(ctx context.Context, ref string) error {
	pullCtx, cancel := context.WithTimeout(ctx, pullTimeout)
	defer cancel()

	if out, err := docker(pullCtx, "pull", "--quiet", ref); err != nil {
		return fmt.Errorf("docker pull %s: %w\n%s", ref, err, out)
	}

	return nil
}

// validatorsSources are the files of the validators image build context:
// source path relative to dir (or the Sourcemeta binary) and name in the
// context.
func validatorsSources(dir, jsonschema string) []struct{ from, to string } {
	return []struct{ from, to string }{
		{filepath.Join(dir, "validators", "Dockerfile"), "Dockerfile"},
		{filepath.Join(dir, "validators", "requirements.txt"), "requirements.txt"},
		{filepath.Join(dir, "validators", ".dockerignore"), ".dockerignore"},
		{jsonschema, "jsonschema"},
	}
}

// validatorsTag derives the tag of the validators image from its Dockerfile,
// requirements and the Sourcemeta binary, so an image with that tag holds
// exactly this content and is reused. The harness never deletes images.
func validatorsTag(dir, jsonschema string) (string, error) {
	h := sha256.New()

	for _, s := range validatorsSources(dir, jsonschema) {
		data, err := os.ReadFile(s.from)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", s.from, err)
		}

		_, _ = fmt.Fprintf(h, "%s\x00%d\x00", s.to, len(data))
		_, _ = h.Write(data)
	}

	return validatorsRepository + ":" + hex.EncodeToString(h.Sum(nil)), nil
}

func buildValidatorsImage(ctx context.Context, dir, jsonschema, artifacts, tag string) error {
	ctxDir, err := os.MkdirTemp("", "schepherd-e2e-validators-")
	if err != nil {
		return fmt.Errorf("create build context: %w", err)
	}

	defer func() { _ = os.RemoveAll(ctxDir) }()

	for _, s := range validatorsSources(dir, jsonschema) {
		data, err := os.ReadFile(s.from)
		if err != nil {
			return fmt.Errorf("read %s: %w", s.from, err)
		}

		if err := os.WriteFile(filepath.Join(ctxDir, s.to), data, 0o755); err != nil {
			return fmt.Errorf("write %s: %w", s.to, err)
		}
	}

	if got, err := validatorsTag(dir, jsonschema); err != nil || got != tag {
		return errors.Join(err, fmt.Errorf("the validators sources changed while building %s", tag))
	}

	// --load: with a builder of the docker-container driver selected
	// (docker buildx use, BUILDX_BUILDER) the image would otherwise stay in
	// the build cache and never reach the image store docker run uses.
	out, err := docker(ctx, "buildx", "build", "--load", "--tag", tag, ctxDir)
	if err != nil {
		log := filepath.Join(artifacts, "validators-build.log")
		if werr := writeFileTo(log, out); werr == nil {
			return fmt.Errorf("docker buildx build: %w (log: %s)", err, log)
		}

		return fmt.Errorf("docker buildx build: %w\n%s", err, out)
	}

	if !imagePresent(ctx, tag) {
		return fmt.Errorf("docker buildx build succeeded, but %s is not in the local image store "+
			"(check the selected builder with 'docker buildx ls')", tag)
	}

	return nil
}

func writeFileTo(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}

	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}
