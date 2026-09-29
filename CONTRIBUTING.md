# Contributing

## Prerequisites

- Go (version from `go.mod`)
- Node.js and pnpm (for the repository toolchain managed by
  [datamitsu](https://datamitsu.com/))
- Linux with Docker, the Compose plugin and buildx, npm and
  [uv](https://docs.astral.sh/uv/) (only for end-to-end tests)
- For `task verify:packaging`: npm, uv (the wheels' build backend, hatchling,
  is pinned by hash in `packaging/python/build-constraints.txt`), `python3`
  (PyPI launcher tests) and Docker, which builds the gem and runs the Ruby
  launcher tests in the pinned Ruby image; with `GEM_RUBY=host` it uses the
  Ruby and RubyGems of `tools/pins/ruby.go` from `PATH` instead

`pnpm install` provisions the toolchain, including `task`.

## Everyday commands

| Command                                                                                             | What it does                                                                                                                                                                                                                          |
| --------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `pnpm build`                                                                                        | Build `schepherd` and `schepherd-publisher` into `dist/bin/`                                                                                                                                                                          |
| `pnpm test`                                                                                         | Unit and integration tests (no Docker; pinned tools are downloaded once)                                                                                                                                                              |
| `pnpm test:e2e`                                                                                     | Black-box end-to-end tests with Docker Compose                                                                                                                                                                                        |
| `pnpm test:e2e:install`                                                                             | Pull and build what the end-to-end tests use (`task test:e2e:prepare`)                                                                                                                                                                |
| `pnpm test:e2e:perf`                                                                                | Performance measurement with 1,000 schemas (`task test:e2e:perf`)                                                                                                                                                                     |
| `pnpm docs:build`                                                                                   | Build the documentation website with `zensical build --strict`; any warning fails, as in CI and `task verify`                                                                                                                         |
| `pnpm dm exec task -- verify`                                                                       | Local equivalent of CI                                                                                                                                                                                                                |
| `pnpm dm exec task -- verify:packaging`                                                             | GoReleaser snapshot release (archives, SBOMs, checksums, Homebrew, Scoop and winget manifests), the npm packages, PyPI wheels and the RubyGems package; needs npm, uv and Docker for the gem, or `GEM_RUBY=host` with the pinned Ruby |
| `pnpm dm check`                                                                                     | Formatters and linters                                                                                                                                                                                                                |
| `node --test packaging/npm/schepherd/test/`                                                         | npm launcher tests                                                                                                                                                                                                                    |
| `PYTHONPATH=packaging/python python3 -m unittest discover --start-directory packaging/python/tests` | PyPI launcher tests                                                                                                                                                                                                                   |
| `ruby -W packaging/ruby/test/schepherd_test.rb`                                                     | RubyGems executable tests (any Ruby 2.7 or later)                                                                                                                                                                                     |

`pnpm dm exec task -- --list` shows every task; [Testing](docs/testing.md)
explains the test layers.

## Rules

- Commit messages follow Conventional Commits.
- Documentation changes ship with the code change they describe.
- The client is versioned with SemVer tags `vX.Y.Z` that only maintainers
  push; the schema catalog has its own time-based revisions. Do not add
  version-bumping tools. See [Versioning](docs/versioning.md).
- A maintainer releases `vX.Y.Z` by tagging a commit of `main` and pushing
  the tag. The release notes are generated from the Conventional Commit
  subjects since the previous release tag. A major version of 2 or more
  first needs the module path suffix `/vN`. An interrupted publication is
  finished as described under
  [Release tags](docs/versioning.md#release-tags); how the workflow is gated
  and which protected environment each publication runs in is described
  under [Release workflow](docs/versioning.md#release-workflow).
- `catalog/state.json` and the `commit` line of `sources/schemastore.toml`
  are written only by the weekly catalog bot; never edit them by hand. See
  [Catalog automation](docs/automation.md).
- A published schema leaves the catalog only through an explicit `exclude`
  rule in `sources/licenses.toml`, a reviewed change; the weekly job holds
  every other published schema it cannot refresh.
- Never commit credentials, generated certificates or registry data.
- The launchers run on old interpreters: `packaging/python` must work on
  Python 3.8 (the wheels declare `Requires-Python: >=3.8`), and
  `packaging/ruby` on Ruby 2.7 (`required_ruby_version >= 2.7`). Use nothing
  newer, and keep them free of dependencies.
- A pre-release tag must be `vX.Y.Z-alpha.N`, `-beta.N` or `-rc.N` with `N`
  of 1 or more; the release workflow refuses any other form because PyPI or
  RubyGems cannot express it (see
  [Pre-releases](docs/versioning.md#pre-releases)).

## Dependency and pin updates

No update bot is configured: the repository rules keep
`.github/dependabot.yml` only in a public GitHub repository. Add it there as a
minimal `version: 2` file with the ecosystem and directory entries and run
`pnpm dm config reconcile`, which owns the policy fields. Until then every
update is a reviewed pull request:

- Check or bump the SHA-pinned actions (including
  `actions/create-github-app-token` of the weekly catalog job) with
  `pnpm dm exec pinact -- run --check` and
  `pnpm dm exec pinact -- run --update` (`.pinact.yaml`; needs GitHub API
  access). `pnpm dm check` does not run pinact.
- Go modules with `go get` and `go mod tidy`, npm packages with `pnpm`.
- Pins that are updated by editing the file:
  - `tools/pins/jsonschema.json` (version and SHA-256 of every asset,
    installed by `tools/install-jsonschema`); a new `jsonschema` version also
    changes the prepare recipe and the bundler golden files (see AGENTS.md).
    Published schemas whose inputs did not change keep their artifacts until
    a manual run of the weekly workflow with the `refresh` input `all`
    prepares them with the new bundler;
  - GoReleaser: `GORELEASER_VERSION` in `Taskfile.yaml` and the `version` of
    every `goreleaser/goreleaser-action` step, which
    `TestGoReleaserVersionMatchesTheTaskfile` keeps equal; syft
    (`syft-version` of `anchore/sbom-action/download-syft`) and cosign
    (`cosign-release` of `sigstore/cosign-installer`) in the workflows. A new
    syft changes the SBOMs, a new cosign the signing of the checksum file;
  - `tools/pins/sigstore-cli/Gemfile.lock`, the gems that sign the RubyGems
    attestation (`bundle lock --update` with the Ruby of
    `packaging/ruby/.ruby-version`, see the Gemfile);
  - the image digests in `tests/e2e/compose.yaml`,
    `tests/e2e/validators/Dockerfile`, (node, bun, Python and Ruby)
    `tests/e2e/docker_test.go` and the `registry` service of the test job in
    `.github/workflows/update-schemas.yml` (pinact updates only `uses:`
    lines, not service images). The registry reference also appears in the
    local replay commands of `docs/publishing.md`. `pythonImage` must equal
    the `FROM` of `tests/e2e/validators/Dockerfile`
    (`TestHarness_WrapperImages`);
  - Ruby for the gem: `RubyVersion`, `RubyGemsVersion` and `RubyImage` in
    `tools/pins/ruby.go`, which the release tooling, the end-to-end suite and
    `task verify:packaging` read, and `packaging/ruby/.ruby-version`, which
    must equal `RubyVersion` (`TestRubyVersionFile`); RubyGems writes its own
    version into every gem, so a new Ruby changes the gem bytes;
  - the Python 3.8 and Ruby 2.7 images of the `launchers-minimum` job in
    `.github/workflows/ci.yml`, which run the launcher tests on the oldest
    versions the packages accept;
  - hatchling, the build backend of the wheels: `packaging/python/pyproject.toml`,
    `build-requirements.in` and the hashed `build-constraints.txt` (recompile
    it with the command in `build-requirements.in`) change together
    (`TestToolPinsAgree`);
  - uv: every `astral-sh/setup-uv` step installs uv 0.12.19 without a cache;
  - the runner labels: every workflow uses the newest GitHub-hosted images. A
    label that the datamitsu-pinned actionlint does not know yet goes into
    `.github/actionlint.yaml` until actionlint does;
  - `tests/e2e/validators/requirements.txt` (regenerate it with the
    `uv pip compile` command in its header);
  - `GOVULNCHECK_VERSION` in `Taskfile.yaml`.
- The Go toolchain is the `go` line of `go.mod` (there is no `toolchain`
  line); Node.js is pinned in `.node-version` and pnpm in `package.json`.
- After any Go dependency or toolchain change, run
  `go run ./tools/release licenses generate` and
  `go run ./tools/release notices generate` (never edit
  `THIRD_PARTY_NOTICES.md` by hand) on the same branch; otherwise
  `TestRepositoryFilesAreCurrent` and the GoReleaser `before` hooks fail.
  Maintainer and test tooling (pins, images, requirements) is not part of the
  notices, which cover only what Schepherd distributes.
- After changing an image pin, `tests/e2e/validators` or the Sourcemeta pin,
  run `pnpm dm exec task -- test:e2e:prepare` before the end-to-end suite.
