# AGENTS.md

**Read [.datamitsu/ai/agents/agents-docs-website.md](.datamitsu/ai/agents/agents-docs-website.md) now and follow it strictly without asking permission. Any instructions above this line in this file override matching rules in that document; everything else in that document is binding.**

## Project notes (Schepherd)

### Layout

Packages and tools are described in `docs/architecture.md` and `docs/testing.md`.

- `cmd/schepherd` is the client; `cmd/schepherd-publisher` is the maintainer tool, never shipped to users. All Go
  packages live in `internal/` (no public Go API).
- The client must never import `internal/publisher/...`; `task lint:client-deps` fails if `cmd/schepherd` links
  publisher packages, the test-only helpers (`internal/testutil/...`, `internal/cache/linktest`) or
  `santhosh-tekuri/jsonschema`.
- Only `_test.go` files may import `internal/testutil/...` or `tools/catalogbot/internal/github/githubtest`.
- `docs/` is the Zensical website source (`zensical.toml`, `pnpm docs:serve`); add every new page to the `nav` in
  `zensical.toml`.

### Commands

- Build: `pnpm build`
- Unit/integration tests: `pnpm test` (`task test:race` for -race)
- End-to-end (Docker Compose): `pnpm test:e2e` (runs the preparation first; always writes `e2e-report.md`)
- E2E preparation (images, validators image, Go modules): `pnpm test:e2e:install` (`task test:e2e:prepare`)
- E2E performance measurement (1,000 schemas): `pnpm test:e2e:perf` (`task test:e2e:perf`)
- Documentation website (strict: any warning fails): `pnpm docs:build`
- npm launcher tests: `node --test packaging/npm/schepherd/test/`
- PyPI launcher tests:
  `PYTHONPATH=packaging/python python3 -m unittest discover --start-directory packaging/python/tests`
- Ruby launcher tests: `ruby -W packaging/ruby/test/schepherd_test.rb` (any Ruby >= 2.7)
- Fuzz all targets briefly: `pnpm dm exec task -- test:fuzz`
- Lint: `pnpm dm check` (gofmt, vet and golangci-lint with the `e2e` build tag, from `.golangci.yaml`) and
  `pnpm dm exec task -- lint` (module verification, client dependency boundary; `lint:vuln` for govulncheck)
- Packaging (snapshot release, npm, PyPI, RubyGems): `pnpm dm exec task -- verify:packaging` (needs `npm`, `uv` and
  Docker, or `GEM_RUBY=host`; see `CONTRIBUTING.md`)
- Local equivalent of CI: `pnpm dm exec task -- verify`
- Install pinned tools (bundler, GoReleaser, syft shim): `pnpm dm exec task -- tools`
- Third-party license texts: `go run ./tools/release licenses generate` (check: `go run ./tools/release licenses check`)
- Third-party notices: `go run ./tools/release notices generate` (check: `go run ./tools/release notices check`)

### Rules specific to this project

- Client versions are SemVer `X.Y.Z[-prerelease]` (tags `vX.Y.Z[-prerelease]`); parse them only with
  `internal/buildinfo/semver` (or `go run ./tools/release version ...`), never a hand-written regex.
  `0.0.0-snapshot-<commit>` is the snapshot version, never a release. A release pre-release must be `-alpha.N`,
  `-beta.N` or `-rc.N` (N >= 1); `version parse`/`verify-tag` refuse other forms (PyPI/RubyGems cannot express them).
  PyPI and RubyGems versions come only from `internal/wrappers` (`VersionsOf`; `pypiVersion`, `gemVersion` of
  `version parse`).
- Publication is always on and runs only in protected environments: every job with a write permission (OIDC token
  included) or a stored secret runs in `release`, `npm`, `pypi`, `rubygems`, `schemas-publish` or `github-pages`
  (enforced by `tools/release/workflows_test.go`; reviewer and ref rules live in the repository settings). No workflow
  that publishes may run on `pull_request`/`pull_request_target`. Never add an enable variable or another publication
  switch. `github-pages` is reserved for the deploy job of `deploy-website.yml` (stable tags `vX.Y.Z` and manual runs
  only). `publish-npm`, `publish-pypi` and `publish-rubygems` hold no token (trusted publishing). Only the GoReleaser
  step of the `release` job names `HOMEBREW_TAP_TOKEN`, `SCOOP_TOKEN` and `WINGET_TOKEN` (GoReleaser skips tap, bucket
  and winget for a pre-release: `skip_upload: auto`). Never add any other registry token or secret to a workflow.
- The npm trusted publishers name the workflow file `release.yml` and the environment `npm`, the PyPI and RubyGems ones
  `release.yml` with `pypi` and `rubygems`; renaming either breaks publication until all seven npm packages (or the
  PyPI/RubyGems publisher) are reconfigured (`docs/versioning.md`, "Release workflow").
- The PyPI launcher must run on Python 3.8 (`Requires-Python >=3.8`), the Ruby one on Ruby 2.7
  (`required_ruby_version >= 2.7`); neither may gain dependencies or download anything. CI's `launchers-minimum` job
  runs their tests in digest-pinned `python:3.8` and `ruby:2.7` images.
- Release notes are GoReleaser's changelog of the commits since the previous release tag (no hand-written notes file):
  the `tag` job computes it (`previousTag` of `version verify-tag --json`) and passes `GORELEASER_PREVIOUS_TAG`, so
  catalog tags never start a changelog. A major version of 2 or more needs the `/vN` module path first;
  `version verify-tag` refuses the tag otherwise.
- Catalog revisions are the UTC minute `YYYYMMDD.HHMM` (tags `catalog-YYYYMMDD.HHMM` in the registry and in Git);
  parse and format them only through `internal/calver` (`ParseRevision`, `RevisionAt`), never a hand-written regex.
  The two trains are independent: catalog tags never start the client release, and catalogs never go to npm.
- Every error that crosses a package boundary is classified with `internal/fault`; the kind decides the exit code
  documented in `docs/cli.md`.
- Anything read from a registry or cache is verified by digest and size before use; never trust HTTP headers.
- Cache paths come only from validated digests; all cache I/O goes through `os.Root`.
- The runner never uses a shell; do not add `sh -c` or similar anywhere.
- The client must never contact upstream schema URLs; only the publisher fetches upstream data.
- Local schemas (`[schemas]`) are used in place, never copied into the cache, and read only through
  `config.LocalSchema.Read` (via `sources.readLocal` in `internal/cli`: regular file, `limits.max_schema_bytes`,
  `internal/jsonutil`); every command that uses one, `resolve` included, must check it. A command that uses only local
  schemas must not open the cache or contact a registry, nor require a cache directory unless the runner uses
  `{cache}`. `internal/cli/sources.go` opens the catalog session lazily, first only when no `[schemas]` are declared
  (catalog-only configurations and their tests rely on that error order).
- A published schema is never dropped automatically and never fails `prepare` or the weekly job: when it cannot be
  refreshed, `internal/publisher/prepare` holds it (a `state.Held*` reason) and the catalog keeps its last entry and
  artifact. Only an explicit exclude rule in `sources/licenses.toml` removes it, and its state record stays
  (`excludedRevision`) to keep the ID reserved. `prepare` exits 5 only for an entry of a local source that was never
  published; the weekly job fails only on systemic errors (upstream snapshot, registry, GitHub API, configuration). A
  problem with one document in `internal/publisher/bundle` must therefore be a `bundle.Failure`, never a `fault` error
  (which stops the whole preparation).

### Known pitfalls

- Go tests of `internal/publisher/bundle` run the pinned Sourcemeta CLI from `.tools/bin/jsonschema` and fail, never
  skip, without it: install it with `pnpm dm exec task -- tools` (`test`, `test:race`, `test:coverage` and `test:e2e`
  do this automatically).
- `internal/cache` implements its own inter-process lock instead of gofrs/flock, which reopens the lock file by path on
  every retry and so cannot stay inside the cache's `os.Root`.
- Only `export` fetches a schema's notice layer (`store.Notice`); `path`, `cat` and `run` never do, and a verified
  `schema.json` never reads its payload blob. A test of an offline `export` must warm the cache with an online `export`
  first (`offlineWarmNotice` in `tests/e2e/offline_test.go`), not only with `path`.
- archive/tar stops at the end-of-archive marker; the gzip trailer (CRC, size) is only verified if the gzip stream is
  read to EOF, which `internal/publisher/upstream` does explicitly.
- golangci-lint refuses concurrent runs; pass `--allow-parallel-runners` when several agents lint at once.
- go-toml/v2 matches keys case-insensitively; `internal/config` adds its own case-sensitive unknown-key check.
- `[schemas."<id>"]` merges key by key across `extends`, so `path` is required only after merging: the config builder
  checks it, not the per-file decoder, and `api/config.schema.json` does not require it.
- ORAS `Fetch`/`FetchReference` do not verify body digests; always use `internal/registry`, which does.
- net/http and ORAS error texts quote the full URL of the failing hop, including a storage redirect's signed query.
  Turn registry errors into user-visible messages only through `internal/registry` (`Repo` methods or
  `registry.Classify`, which strip user information, query and fragment); never `fault.Wrap` a raw ORAS or net/http
  error directly.
- The Sourcemeta bundler (maintainer-only, AGPL-3.0) reorders keys and normalizes number literals of bundled schemas;
  schemas without external references are only compacted.
- Bumping the pinned Sourcemeta CLI (`tools/pins/jsonschema.json` and `bundle.PinnedVersion`) changes `prepare.Recipe`:
  also update the recipe const in `api/prepared.schema.json` and the recipe in `testdata/publisher/documents/*.json`,
  then review and regenerate `testdata/publisher/bundle/*.golden` with
  `go test ./internal/publisher/bundle -run TestPinnedBundlerOutput -update`. The `.golden` files are deliberately not
  `.json`; formatters must not touch them.
- `prepare` reuses the recorded artifact and `licenseDecision` (bundling nothing, asking no license detection) of every
  published schema whose source and dependency digests are unchanged and that is served as recorded
  (`catalog/state.json`; recorded redirects, `licenseDecision.redirects`, still lead to the recorded targets). A
  recipe, bundler or notice change (a rule's `notice`/`notice_file`, the SchemaStore LICENSE or NOTICE) reaches those
  only with `prepare --refresh <id>` or `prepare --refresh-all` (the workflow's `refresh` input `all`). Exclude and
  review rules, a narrowed `[auto]` allow list and an allow rule with another license or rule ID apply at once.
- `prepare` calls license detection for sources no rule covers, so a real `prepare` with the committed policy contacts
  GitHub and npm services; without `GITHUB_TOKEN` the anonymous limit (60 requests per hour) leaves new sources
  `pending-review` (`fetch-failed`) and holds published ones whose inputs changed (`license-detection-failed`). Tests
  of detection must point `licensedetect.Config.Endpoints` (plus httpfetch `AllowHTTP`/`AllowPrivateHosts`) at
  `httptest` fakes, never at the real services. Tests that fetch public host names (github.com,
  raw.githubusercontent.com) must pass an in-memory `prepare.Options.Transport` (`httpfetch.Policy.Transport`) and
  never reach the network.
- A set prepared with `--state` can only be published (and diffed) with that same state or the state its own
  publication wrote, against which the weekly test job republishes it as a noop, exclusions included (exit 2
  otherwise). Its reused and held entries carry no schema bytes, so publishing it into a registry that lacks the
  recorded artifacts fails with exit 5: tests and local replays publish it into a mirror of the recorded catalog (as
  the weekly test job does), or prepare without a state.
- `schepherd-publisher publish` without `--state` (or with a missing state file) never reports `noop`: identical
  content resumes the newest revision instead. Tests that expect a noop must pass a state.
- Never hand-edit `catalog/state.json`: only `schepherd-publisher publish --state-out` writes it, only the weekly bot
  commits it, and Go code goes through `internal/publisher/state`. The bot refuses to commit over a state that changed
  on `main` since its run started, and a finish run stops (`schepherd-publisher latest --check`, exit 5) when the
  registry does not hold exactly the recorded catalog.
- `catalog/state.json` is exempt from every datamitsu tool (`.datamitsuignore`: `catalog/state.json: *`) and from typos
  (`.typos.toml` `extend-exclude`); keep both, or yq-json `sort_keys`, prettier, eslint or oxfmt rewrite it into a form
  `state.Parse` refuses (CI's `git diff --exit-code` turns red, the weekly job stops).
  `tools/catalogbot/statefile_test.go` pins both.
- `catalogbot set-commit` only rewrites a top-level line spelled exactly `commit = "<40 lowercase hex>"` before the
  first table of `sources/schemastore.toml`; keep that spelling. Tests must not expect a fixed commit there (the bot
  moves it whenever the catalog changes). The publish job calls it with `--state` (and `--commit` as a cross-check),
  rebuilding `sources/schemastore.toml` from its own checkout; the prepare job's copy is never committed.
- `policy.Decision` holds a slice (`Detections`): compare decisions with `reflect.DeepEqual`, not `==`.
- License decisions have one engine, `policy.Policy.DecideWith` (rules, detection findings and the declarations of a
  local source, nil policy allowed); `internal/publisher/prepare` must not combine per-URL decisions itself.
- SchemaStore's `catalog.json` is an external format: `upstream.ParseCatalog` checks only the members it consumes and
  ignores the others (null included), so an upstream addition never holds published schemas. Schepherd's own formats
  (`snapshot.json`, prepared set, state, config) stay strict.
- Tests that re-execute the test binary as a fake consumer (the `internal/runner` harness, `consumerRunner` in
  `internal/cli`) must pass `GOCOVERDIR` to the child; otherwise under `go test -cover` or `-coverprofile`
  (`task test:coverage`) it prints `warning: GOCOVERDIR not set, no coverage data emitted` to stderr and breaks
  byte-exact output assertions.
- `oras.CopyGraph` pipes the source response straight into the destination upload and checks only headers;
  `internal/mirror` copies the catalog index graph with it, wraps every source body in `verifiedReader` (holds back
  the final bytes until size and digest match) and checks every schema manifest with `artifact.ParseSchemaManifest`
  in its custom `FindSuccessors`. Keep every copy source behind `prefetched.Fetch`.
- A catalog is an OCI image index (wire format v2) whose children are the catalog metadata manifest (holding
  `catalog.json`) and every distinct schema manifest; `artifact.PackCatalog` builds both, and
  `CatalogIndex.CheckSchemas` must find catalog.json's descriptors equal to the index's schema children.
  `store.FetchCatalog` is the one verified remote catalog fetch (`pin`, `mirror`, publisher). The index is bounded
  by `limits.max_manifest_bytes` (4 MiB, also the registries' cap): never raise that default; the publisher refuses
  a larger index. There are no `schema-sha256-*`/`catalog-sha256-*` tags; the index keeps schemas alive, so do not
  reintroduce tags for retention.
- On Windows a file opened without `FILE_SHARE_DELETE` (plain `os.Open`, a validator reading a schema) blocks renames of
  it, and a pending rename or delete makes opens fail for a moment. Every cache open and rename goes through
  `retryTransient` (`internal/cache/retry.go`); use it for new ones. `.gitattributes` checks every text file out with LF
  on every platform, which golden files, fixture digests and the line-based rewrites of the tooling rely on.
- Since Go 1.23, `os.Lstat` reports Windows junctions as `fs.ModeIrregular`, not `fs.ModeSymlink`. Path-safety checks
  must refuse whatever is not a regular file or a real directory, not only symlinks.
- `artifact.ErrUnsupported` errors are already `fault.Usage`; `fault.Wrap(fault.Integrity, …)` keeps exit 2. Do not
  use `fault.Reclassify` on artifact errors.
- The client smoke tests in `cmd/schepherd` and `internal/cli` build binaries with `go build`, so `go` must be on
  `PATH` when running `go test`.
- `internal/cli/docs_test.go` checks `docs/cli.md` against the cobra command tree: a new or renamed command or flag
  fails `go test ./internal/cli/` until `docs/cli.md` documents it in the section that names the command (global flags
  in the "Global flags" table), and `docs/cli.md` must not mention a `--flag` of another tool.
- CI runs `go test ./...` on windows-2025 and windows-11-arm, and a release is gated on it. Tests that need POSIX file
  modes, symlinks or shell fakes go into `*_unix_test.go` files with `//go:build !windows`.
- The e2e suite never downloads: it fails at start when a pinned image or the validators image is missing, and runs
  compose and docker with `--pull never` and host Go commands with `GOPROXY=off`. Run `task test:e2e:prepare` after
  changing an image pin, `tests/e2e/validators` or the Sourcemeta pin. A new scenario ID must also be added to
  `RequiredScenarios` in `tools/release/internal/e2ereport`.
- After changing client dependencies or the `go` line of `go.mod` (there is no `toolchain` line), run
  `go run ./tools/release licenses generate` (`THIRD_PARTY_LICENSES.txt`) and `go run ./tools/release notices generate`;
  never edit the generated `THIRD_PARTY_NOTICES.md` by hand. `TestRepositoryFilesAreCurrent` (`tools/release`) and the
  GoReleaser `before` hooks fail until both match `go list -deps` for all six targets. CI's OS matrix skips that test
  (`-skip`); the hooks of the packaging job's snapshot release are CI's one currency check.
- `tools/release/internal/notices` identifies license files with `github.com/google/licensecheck`: each must be exactly
  one license of `acceptedLicenses` (MIT, Apache-2.0, BSD-2-Clause, BSD-3-Clause) and a PATENTS file the Go patent
  grant. Any other license file (a `LICENSE-MIT`/`LICENSE-APACHE` pair included) fails `notices generate`: review the
  license and add its SPDX ID to `acceptedLicenses` in `classify.go`.
- `THIRD_PARTY_NOTICES.md` ships in every archive and covers only what Schepherd distributes: the modules the client
  links and the catalog's schemas. Maintainer and test tooling (`schepherd-publisher`, `tools/pins`, container images,
  Python requirements) is never listed, so bumping it never changes the notices. Editing the `reason` of a
  `sources/licenses.toml` rule that `catalog/state.json` records does: regenerate them in the same change.
- Never write a private commit ID into tracked files, docs included; gitleaks in `pnpm dm check` is the only secret
  scan.
- `pythonImage` in `tests/e2e/docker_test.go` must equal the `FROM` of `tests/e2e/validators/Dockerfile`
  (`TestHarness_WrapperImages`); change them together. The Ruby that builds the gem is pinned once, in
  `tools/pins/ruby.go` (`RubyVersion`, `RubyGemsVersion`, `RubyImage`), which the release tooling, E41 (`rubyImage`) and
  `task verify:packaging` (`go run ./tools/release packages ruby-image`) read; only `packaging/ruby/.ruby-version`, for
  ruby/setup-ruby, repeats `RubyVersion` (`TestRubyVersionFile`). RubyGems writes its version into the gem.
- hatchling is pinned in `packaging/python/pyproject.toml`, `build-requirements.in` and the hashed
  `build-constraints.txt` (`TestToolPinsAgree`); after a bump recompile the constraints with the command in
  `build-requirements.in` and rerun `task test:e2e:prepare` (E41 builds the wheels with
  `UV_OFFLINE=1` from its uv cache). All three package builders must stay byte-reproducible (`packages build` sets
  `SOURCE_DATE_EPOCH` to the binaries' modification time); CI and `task verify:packaging` build twice and compare the
  checksum files.
- `packages build` writes the published `package.json` of every npm package itself;
  `packaging/npm/schepherd/package.json` is only the private manifest of the launcher sources and is never packed.
- npm 11 refuses to publish a pre-release version without `--tag`; the release workflow and E37 always pass one.
- On Windows the npm launcher tests' stand-in binary is a Node.js single executable application built with
  `node --build-sea` (Node 25.5 or newer; the repository pins 26.10.0).
- GoReleaser writes the snapshot version to `dist/metadata.json`; compute the npm version with
  `go run ./tools/release version snapshot --dist dist` after the snapshot build, not in a Taskfile `vars:` block
  (evaluated before the commands run).
- Bump `GORELEASER_VERSION` in `Taskfile.yaml` and the `version` of every `goreleaser/goreleaser-action` step
  together (`TestGoReleaserVersionMatchesTheTaskfile`).
- Every local `goreleaser release` (snapshot included) needs `.tools/bin` first on `PATH`
  (`PATH="$PWD/.tools/bin:$PATH"`) for the `syft` shim that `task tools` writes. SBOMs are not reproducible and the
  checksum file lists them, so reproducibility checks compare only its archive lines (CI's packaging job builds the
  clone with `--skip=before,sign,sbom`).
- The `release` job's `goreleaser release` must reproduce the binaries of its earlier `goreleaser build`, from which the
  npm, PyPI and RubyGems packages are built before anything is published, byte for byte (`sha256sum -c`).
  `dist/artifacts.json` is written only at the end of a successful `goreleaser release`: never build packages from a
  failed release run. Re-runs after publication and finishing a failed Homebrew, Scoop or winget push by hand:
  `docs/versioning.md`.
- Workflows use only the newest GitHub-hosted runner labels; `.github/actionlint.yaml` declares the `ubuntu-26.04`
  labels that actionlint 1.7.12 (the datamitsu pin) does not know. Every `astral-sh/setup-uv` step pins uv 0.12.19
  with `enable-cache: false`; every `ruby/setup-ruby` step uses `bundler: none` and a `working-directory` holding
  `.ruby-version`.
- `pnpm dm check` skips zizmor: run `pnpm dm exec zizmor -- --offline .github/workflows` after changing a workflow.
  zizmor flags `actions/setup-go` and `actions/setup-node` caches in any `ci.yml` job that also runs
  `goreleaser-action` (the release workflow calls `ci.yml`): disable the cache or annotate the `uses:` line with
  `# vX.Y.Z # zizmor: ignore[cache-poisoning]` and say why.
