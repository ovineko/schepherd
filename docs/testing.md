# Testing

| Layer                  | Command                                           | Needs                                                                                                                                                                                                                                                                                                                 |
| ---------------------- | ------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Unit and integration   | `pnpm test` (or `go test ./...`)                  | Go (the `go` command on `PATH`: the client smoke tests build binaries); `.tools/bin/jsonschema` for publisher tests                                                                                                                                                                                                   |
| Race detector          | `pnpm dm exec task -- test:race`                  | Go with cgo (a C toolchain). CI runs it on every runner except windows-11-arm, where Go has no race detector; on windows-2025 it uses the image's MinGW-w64 gcc (`CGO_ENABLED=1`)                                                                                                                                     |
| Coverage               | `pnpm test:coverage`                              | Go; writes `coverage.out`                                                                                                                                                                                                                                                                                             |
| Fuzzing (bounded)      | `pnpm dm exec task -- test:fuzz`                  | Go; `FUZZTIME` per target (default `20s`)                                                                                                                                                                                                                                                                             |
| E2E preparation        | `pnpm test:e2e:install` (`task test:e2e:prepare`) | Linux, Docker with buildx, network (Docker Hub, PyPI, Go module proxy)                                                                                                                                                                                                                                                |
| End-to-end             | `pnpm test:e2e`                                   | Linux, Docker with Compose and buildx, npm, `task tools`, the preparation step (run automatically)                                                                                                                                                                                                                    |
| E2E performance        | `pnpm test:e2e:perf` (`task test:e2e:perf`)       | same as end-to-end                                                                                                                                                                                                                                                                                                    |
| Packaging              | `pnpm dm exec task -- verify:packaging`           | Node.js, npm, uv (it fetches the pinned hatchling from PyPI), `python3` (PyPI launcher tests), Docker for the gem and the Ruby launcher tests (the pinned Ruby image) or `GEM_RUBY=host` with the Ruby and RubyGems of `tools/pins/ruby.go` on `PATH`, `pnpm install` (syft via datamitsu), GoReleaser (`task tools`) |
| Local equivalent of CI | `pnpm dm exec task -- verify`                     | all of the above (CI adds the OS matrix)                                                                                                                                                                                                                                                                              |

`pnpm dm exec task -- tools` installs the pinned maintainer tools into
`.tools/bin`: the Sourcemeta bundler (`go run ./tools/install-jsonschema`,
which verifies its SHA-256 checksum) and GoReleaser `GORELEASER_VERSION`
(`go install`, verified against the Go checksum database), and writes a
`syft` shim that runs the syft datamitsu pins (`pnpm dm exec syft`).
GoReleaser runs syft from `PATH` for the SBOM of every archive, so every
local `goreleaser release` puts `.tools/bin` first in `PATH`; CI and the
release workflow install GoReleaser, syft and cosign with their pinned
actions instead. Nothing else is downloaded implicitly, apart from Go modules
missing from the module cache (see
[Which stages need the network](#which-stages-need-the-network)).

How dependencies and pinned tools are updated is described in
[CONTRIBUTING.md](https://github.com/ovineko/schepherd/blob/main/CONTRIBUTING.md#dependency-and-pin-updates).

## Unit and integration tests

They run without Docker and without network access (local `httptest`
servers and in-memory registries on `127.0.0.1` only) and never touch files
outside their temporary directories. They cover, among others:

- the client: configuration merging, precedence and interpolation through the
  real command line, local schemas, matching, runner planning and process
  cleanup (via a helper process), cache atomicity, locks, corruption recovery
  and symlink and junction attacks, registry transport (digest verification
  despite lying headers, credential scoping, TLS with a private CA, retries,
  timeouts, redaction of presigned URLs from errors) and mirroring against a
  destination that accepts unverified uploads;
- the wire format: catalog parsing and golden fixtures validated against
  `api/catalog.schema.json`, lossless JSON compaction, deterministic packing
  with golden digests, and refusal of other format versions as unsupported;
- the client smoke test on every platform: `cmd/schepherd` builds the real
  binary and a stand-in consumer with the `go` command on `PATH`, publishes a
  two-schema catalog to an in-memory registry and runs every command as a
  subprocess, including a warm cache that makes no registry request and an
  offline miss that exits 6. The same scenario runs in process through
  `cli.Main` under the race detector;
- command reference drift: `internal/cli/docs_test.go` fails when
  `docs/cli.md` is missing a command or flag or names one that does not
  exist. Global flags belong in the "Global flags" section; every other flag
  in a section that names its command in code format, at the start of a
  heading or in a table's first cell;
- the publisher: SSRF protections, tarball safety, stable IDs, license
  policy and automatic detection against `httptest` fakes of the GitHub API,
  the npm registry and the CDNs, bundling with differential validation and
  golden files of the pinned bundler, the state and the plan shared by `diff`
  and `publish`, revision allocation and publish ordering. `TestWeeklyFlow`
  and `TestWeeklyPipeline` run `prepare` and `publish` week after week: an
  unchanged week makes no detection request and no registry write, and
  failures hold schemas instead of failing the run. Tests that fetch public
  host names serve them from memory through `prepare.Options.Transport`;
- the catalog bot (`go test ./tools/catalogbot/...`) against
  `githubtest`, an `httptest` fake of the GitHub API, including
  `pipeline_test.go`, which runs the publish job step by step with the real
  publisher, and `statefile_test.go`, which keeps every datamitsu tool and
  typos away from `catalog/state.json`;
- the release tooling (`go test ./tools/...`): version and tag checks, the
  package builds and publication plans against fake registries, the tool pins
  that must agree, `TestRepositoryFilesAreCurrent` (it needs the module
  cache, or the network for modules only Windows links; CI's OS matrix skips
  it, see [Third-party license texts and notices](#third-party-license-texts-and-notices)),
  the workflow policy, and `TestModuleTagInstalls`, which installs the client
  with `go install …@<tag>` from a throwaway tagged repository and needs `git`
  and a module cache that holds the client's dependencies;
- the launchers: `node --test packaging/npm/schepherd/test/`, the PyPI
  launcher tests (`PYTHONPATH=packaging/python python3 -m unittest discover
--start-directory packaging/python/tests`) and
  `ruby -W packaging/ruby/test/schepherd_test.rb`: platform selection, the
  `SCHEPHERD_BINARY` override, and arguments, standard input, signals and
  exit status passed through. On Windows the npm tests' stand-in binary is a
  Node.js single executable application, which needs Node.js 25.5 or newer
  (the repository pins 26.10.0).

CI runs `go test ./...` on windows-2025 and windows-11-arm, and a release is
gated on it; tests that need POSIX file modes, symlinks or shell fakes live
in `*_unix_test.go` files with `//go:build !windows`.

Fuzz targets cover every parser of untrusted input: catalog JSON, digests and
references, interpolation templates, configuration TOML, manifests, gzip
payload decoding and JSON validation, plus SemVer parsing and the code spans
of the catalog release notes (`FuzzCodeSpan`). `task test:fuzz` finds every `Fuzz…` target by itself.

Golden files are regenerated deliberately, never to make a test pass:

| Files                                | Command                                                                    |
| ------------------------------------ | -------------------------------------------------------------------------- |
| `testdata/catalogs/*`                | `go test ./internal/catalog -run TestGolden -update`                       |
| `testdata/publisher/bundle/*.golden` | `go test ./internal/publisher/bundle -run TestPinnedBundlerOutput -update` |
| `THIRD_PARTY_LICENSES.txt`           | `go run ./tools/release licenses generate`                                 |
| `THIRD_PARTY_NOTICES.md`             | `go run ./tools/release notices generate`                                  |

## End-to-end suite

`tests/e2e` is a black-box suite (build tag `e2e`). It starts real OCI
registries (CNCF Distribution `distribution/distribution:3.1.2`, pinned by digest) with Docker
Compose, builds `schepherd` and `schepherd-publisher` from the working tree
and runs them as subprocesses. A counting, fault-injecting reverse proxy in
front of each plain registry proves claims such as "no network request" from
recorded traffic rather than from cache sizes.

```bash
pnpm dm exec task -- test:e2e:prepare   # once per machine and after an image pin, tests/e2e/validators or the Sourcemeta pin changed
pnpm test:e2e                           # runs the preparation step first, then the suite and the report
# after preparation also: go test -tags e2e -count=1 -timeout 60m ./tests/e2e/...
```

Services (`tests/e2e/compose.yaml`):

| Service                           | Role                                                              |
| --------------------------------- | ----------------------------------------------------------------- |
| `registry-source`                 | stands in for a public upstream registry                          |
| `registry-mirror`                 | stands in for a private corporate mirror                          |
| `registry-auth`, `registry-auth2` | htpasswd + TLS with a CA generated per run, different credentials |
| `verdaccio`                       | local npm registry for packaging tests (profile `npm`)            |

Every run uses its own Compose project name, its own temporary directories and
loopback-only ports. Readiness is probed by the test driver over HTTP (`/v2/`
on the registries, `/-/ping` on Verdaccio, every 200 ms for up to 90 s).
Compose healthchecks are disabled, so readiness never depends on a tool such
as wget, curl or a shell inside an image. On failure the Compose logs are
saved to `tests/e2e/.artifacts/<project>` (or `E2E_ARTIFACTS_DIR`) before
`docker compose down -v --remove-orphans` removes the project's containers,
networks and volumes. Nothing outside the project is touched. Without Docker
the suite fails with an explanation instead of being skipped.

Real validators run in a locally built image
(`tests/e2e/validators/Dockerfile`: `check-jsonschema` 0.38.2 installed from a
hash-locked requirements file, plus the pinned Sourcemeta CLI) with
`--network none`, so the offline claims are checked with the network really
absent. The image is built by the [preparation step](#preparation).

### Preparation

`task test:e2e:prepare` runs `go mod download` and then
`E2E_PREPARE=1 go test -tags e2e -count=1 -timeout 30m -run '^$' -v ./tests/e2e`,
which runs no test: it pulls every pinned image the scenarios use, builds the
validators image, puts the hash-pinned hatchling into uv's cache (E41 builds
the wheels offline), skips images that are already present, and prints one
line per image (`present`, `pulled` or `built`). A failed validators build leaves
`validators-build.log` in `tests/e2e/.artifacts/prepare` (or
`E2E_ARTIFACTS_DIR`).

The scenarios download nothing. A run first checks that every image is in the
local image store; otherwise it fails before starting anything and names each
missing image and the preparation command. Compose starts services with
`--pull never`, every `docker run` of the harness uses `--pull never`, and the
Go commands the harness runs on the host (the builds, `go run ./tools/release`,
GoReleaser) run with `GOPROXY=off`.

### Scenario matrix

The scenario IDs are stable test names (`TestE01_…` to `TestE42_…`). A
scenario may have more than one test with its ID; it passes only when all of
them pass.

| Group                    | Scenarios                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| ------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Artifacts, lazy fetching | E01 round trip, E02 catalog only, E03 only the requested schema, E04 warm cache without any request, E05 `cat`/`export`, E06 compression                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| Pinning and updates      | E07 one changed schema, E08 no-op publish, E09 mutable tag vs pin, E10 time and host independence                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| Mirror                   | E11 full mirror, E12 source switched off, E13 repeated mirror, E14 interrupted mirror, E15 retention through index references (only catalog tags; the index references every schema manifest `catalog.json` lists), E42 generic OCI copy (plain ORAS `oras.Copy` with default options, then `pin` and every schema byte-identical from the copy)                                                                                                                                                                                                                                 |
| Offline and integrity    | E16 strict offline, E17 offline miss, E18 cache corruption, E19 corrupted transport, E20 resource limits, E21 concurrency, E22 path safety                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| Configuration and runner | E23 batch grouping, E24 per-file, E25 stdin, E26 env and cwd, E27 no shell, E28 interpolation, E29 extends, E30 exit codes and cancellation, E31 real validator (including the shipped examples run unchanged), E32 backend swap, E40 local schemas (validated by check-jsonschema offline, without registry or cache)                                                                                                                                                                                                                                                           |
| Discovery, auth, release | E33 matching, E34 auth and TLS, E35 catalog revisions (noop, resume, same-minute and clock-behind refusals, holds, exclusions, finishing from the state), E37 npm packaging (a snapshot published to Verdaccio through `packages publish-plan`, interrupted and resumed; npm and Bun launchers), E38 self-contained bundle, E39 dependency-only change, E41 PyPI and RubyGems packages (installed offline into the pinned Python and Ruby images; each launcher runs exactly the GoReleaser binary of its platform and passes arguments, standard input and exit status through) |

### Report

`task test:e2e` pipes `go test -json` into
`go run ./tools/release e2e-stream --save <dir>/e2e.json --report <dir>/e2e-report.md --strict`,
where `<dir>` is `E2E_ARTIFACTS_DIR` (default `tests/e2e/.artifacts/latest`).
`e2e.json` is written as the stream arrives. `e2e-report.md` is written when
the stream ends, also when tests fail or the run is interrupted.

The report first lists every required scenario. A scenario is
`PASS` when every top-level test of its ID passed, and `FAIL` when any of its
tests or subtests failed or did not finish. It is `NOT RUN` when no test of
the ID ran (filtered with `-run`, renamed or deleted) or one of its tests was
skipped. Every test and subtest with its duration follows. The task exits
non-zero when any test failed and, because of `--strict`, also when a
required scenario did not pass.

`go run ./tools/release e2e-report --in e2e.json --out report.md [--strict]`
renders the same report from a saved stream. The matrix lives in
`tools/release/internal/e2ereport` (`RequiredScenarios`): a new scenario ID
must be added there, and its unit test fails when a required ID has no
`TestE<ID>_` test or a test uses an ID outside the matrix.

### Performance measurement

`pnpm dm exec task -- test:e2e:perf` runs the preparation step and then
`E2E_PERF=1 go test -tags e2e -count=1 -timeout 60m -run '^TestPerf_' -v ./tests/e2e`.
`TestPerf_SyntheticCatalog` prepares and publishes 1,000 synthetic schemas
with the real publisher and measures `prepare`, `publish`, `catalog --json`
with a cold cache, `list`, `patterns` and `resolve` with the catalog cached,
`path` with the schema cold, `path` warm, `path` with an empty cache, a full
`mirror` and a repeated `mirror`, all through counting proxies.

Results go to `E2E_ARTIFACTS_DIR/perf/` (default
`tests/e2e/.artifacts/latest/perf/`) as `TestPerf_SyntheticCatalog.json`
(per step: duration, exit status, output and cache size, and requests and
bytes per registry proxy) and the same numbers as a Markdown table. There are
no wall-clock assertions. The test does assert that the warm `path`
makes no request and that the repeated mirror writes nothing to the
destination. Without `E2E_PERF` the test only logs that it is off and passes.
One run on linux/amd64 against a local registry gave:

| Step                                            | Duration | Registry requests                 | Transfer                                                                      |
| ----------------------------------------------- | -------- | --------------------------------- | ----------------------------------------------------------------------------- |
| prepare                                         | 0.76 s   | 0                                 | –                                                                             |
| publish (1,000 schemas)                         | 18.6 s   | 12,047                            | 2,502,686 B sent, 883,227 B received                                          |
| `catalog --json`, cold cache                    | 28 ms    | 2                                 | 545,492 B received (544,951 B catalog blob)                                   |
| `list` / `patterns` / `resolve`, catalog cached | 20–26 ms | 0                                 | 0 B                                                                           |
| `path`, schema not cached                       | 38 ms    | 2                                 | 1,074 B received (192 B blob)                                                 |
| `path`, warm                                    | 19 ms    | 0                                 | 0 B                                                                           |
| `path`, empty cache                             | 41 ms    | 4                                 | 546,567 B received                                                            |
| full mirror                                     | 33.8 s   | 2,004 source + 11,016 destination | source: 1,619,254 B received (736,713 B blobs); destination: 2,502,336 B sent |
| repeated mirror (no-op)                         | 0.24 s   | 2 source + 2,003 destination      | source: 545,492 B received (the catalog again); destination: 0 B sent         |

### Troubleshooting

- _Docker is not available_: start the Docker daemon; the suite needs
  `docker` and `docker compose` on `PATH`.
- _Leftovers after an interrupted run_: normally the harness's reaper removes
  them. If it could not, follow the cleanup commands in
  [`tests/e2e/README.md`](https://github.com/ovineko/schepherd/blob/main/tests/e2e/README.md),
  which cover the Compose project, the harness's `docker run` containers and
  the project network.
- _A run fails with "image(s) the scenarios use are not in the local image
  store"_: run `pnpm dm exec task -- test:e2e:prepare`. Images are only
  downloaded there. It is needed again after an image pin,
  `tests/e2e/validators` or the Sourcemeta pin changed. A missing Go module
  fails with "module lookup disabled by GOPROXY=off" for the same reason.

## Release outputs

`goreleaser release --snapshot --clean --skip=sign` (in `task verify:packaging`
and the CI packaging job) builds what a release publishes without signing or
publishing it: the archives, their SBOMs and checksum file, the Homebrew cask
(`dist/homebrew/Casks/schepherd.rb`), the Scoop manifest
(`dist/scoop/bucket/schepherd.json`) and the winget manifests
(`dist/winget/manifests/o/ovineko/schepherd/<version>/`).

### npm, PyPI and RubyGems packages

```bash
go run ./tools/release packages build --dist dist --version <SemVer> --out <new dir> \
  [--kinds npm,pypi,gem] [--all-targets=false] [--ruby host|docker] [--smoke]
go run ./tools/release packages publish-plan --kind npm|pypi|gem --dir <dir> \
  --checksums <file> --version <SemVer> [--registry <URL>]
```

`packages build` stages the GoReleaser binaries of `dist/artifacts.json`
(each checked for object format, CPU, static linking on Linux and the minimum
macOS version) with `packaging/`, `LICENSE` and `THIRD_PARTY_LICENSES.txt` and
builds the packages with the standard builders:

| Kind | Builder                                                                                                      | Output                                                                                        |
| ---- | ------------------------------------------------------------------------------------------------------------ | --------------------------------------------------------------------------------------------- |
| npm  | `npm pack --ignore-scripts` of a generated `package.json` per package                                        | `<out>/npm/`: one platform package per target, then `@ovineko/schepherd`                      |
| pypi | `uv build --wheel` with the hash-pinned hatchling; the hook `hatch_build.py` sets the target's platform tags | `<out>/pypi/schepherd-<PEP 440>-py3-none-<platform tags>.whl`, one per target, no sdist       |
| gem  | `gem build --strict` of `packaging/ruby/schepherd.gemspec`                                                   | `<out>/gem/schepherd-<RubyGems version>.gem` with the binary of every target under `libexec/` |

`SOURCE_DATE_EPOCH` is set to the binaries' modification time, so the same
binaries give byte-identical packages. Each package must hold exactly its
staged files (plus the metadata its builder writes), and
`<out>/schepherd_<version>_<kind>_checksums.txt` lists the packages in
publication order. `--smoke` installs the packages of the running platform
offline and requires each launcher to give exactly the output and exit status
of the binary. Ruby runs on the host (`--ruby host`, the default) or in the
pinned Ruby image (`--ruby docker`, with `--network none`); either must be
exactly the Ruby and RubyGems of `tools/pins/ruby.go`.
`go run ./tools/release packages ruby-image` prints that image. `--all-targets=false` accepts a
single-target build.

`packages publish-plan` requires `--dir` to hold exactly the packages the
checksum file lists for the version, asks the registry (npmjs.org, PyPI,
rubygems.org, or `--registry`) which it already has, and prints the files
still to publish. It fails for a file the registry has with other bytes and
for a snapshot sent to PyPI or RubyGems. Both commands exit with 0 on
success, 1 on a failed check and 2 on usage errors.

`task verify:packaging` runs `goreleaser check`, a snapshot release of every
target (whose `before` hooks run `licenses check` and `notices check`), the packages built
twice and compared, checked and installed, and the npm, PyPI and Ruby
launcher tests (Ruby in the pinned image unless `GEM_RUBY=host`).

### Third-party license texts and notices

```bash
go run ./tools/release licenses generate [--root .] [--out THIRD_PARTY_LICENSES.txt]
go run ./tools/release licenses check [--root .]
go run ./tools/release notices generate [--root .] [--state catalog/state.json] [--out THIRD_PARTY_NOTICES.md]
go run ./tools/release notices check [--root .]
```

`THIRD_PARTY_LICENSES.txt`, shipped in every archive and package, holds the
license, NOTICE and PATENTS texts of the Go standard library and of every
module linked into `./cmd/schepherd` for the six targets, computed with
`go list -deps` and the toolchain named in `go.mod`.

`THIRD_PARTY_NOTICES.md` is generated, never edited by hand, and ships in
every archive. It covers only what Schepherd distributes: the modules the
client links, with the license identified from each license file, and the
schemas of the catalog the state records, grouped by their sources and
licenses; the SchemaStore rules and NOTICE come from
`tools/release/notices.toml`. What only maintainers, CI and tests use
(`schepherd-publisher`, `tools/pins`, container images, Python requirements)
is never distributed and deliberately not listed, so bumping it never changes
the notices; the headline license of an image would not describe everything
inside it anyway. A license file it cannot identify fails `generate`.

Both `check` commands exit with 1 and name what differs when the committed
file does not match what `generate` writes. They run as GoReleaser `before`
hooks, which in CI makes the packaging job's snapshot release the one
currency check, and as `TestRepositoryFilesAreCurrent` in local test runs
(CI's OS matrix passes `-skip '^TestRepositoryFilesAreCurrent$'`); the weekly
catalog job regenerates the notices for every catalog revision it records.

## Workflow policy

`tools/release/workflows_test.go` reads every workflow in
`.github/workflows`: a job with any write permission (`id-token: write` and
`attestations: write` included) or a stored secret must name one of the
environments `release`, `npm`, `pypi`, `rubygems`, `schemas-publish` or
`github-pages`, and no workflow grants write permissions to all its jobs. The
test cannot see the repository settings: those environments must require a
reviewer and allow only their tags or branches. It also requires every
`goreleaser/goreleaser-action` step to run `GORELEASER_VERSION` of
`Taskfile.yaml`. Pinning of actions by commit SHA and the other workflow
rules are left to zizmor (`pnpm dm exec zizmor -- --offline .github/workflows`)
and actionlint (in `pnpm dm check`).

## Continuous integration

`.github/workflows/ci.yml` runs on pull requests and pushes to `main`, and is
also a reusable workflow (`on.workflow_call`): the release workflow calls it,
so a release is built only after all of these jobs passed for the tagged
commit.

| Job               | What it runs                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| ----------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| lint              | `task lint` (`go mod verify`, `go mod tidy -diff`, the client dependency boundary) and `task lint:vuln` (`govulncheck`); formatting, `go vet` and golangci-lint (with the `e2e` build tag of `.golangci.yaml`) run once, in `pnpm dm check` of repository-checks                                                                                                                                                                                                                                                                                                                                |
| test              | ubuntu-26.04, ubuntu-26.04-arm, macos-26, macos-26-intel, windows-2025, windows-11-arm: `go test -race ./...` with `CGO_ENABLED=1` (plain `go test ./...` on windows-11-arm), both skipping `TestRepositoryFilesAreCurrent`, the npm, PyPI and RubyGems launcher tests, a native smoke, and on every runner except Linux x64 the npm, PyPI and RubyGems launcher smokes                                                                                                                                                                                                                         |
| fuzz              | `task test:fuzz` with `FUZZTIME=15s`: every fuzz target for 15 s                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| launchers-minimum | the PyPI launcher tests in a digest-pinned Python 3.8 image and the Ruby launcher tests in a digest-pinned Ruby 2.7 image, without network: the oldest versions the wheels and the gem accept, which `actions/setup-python` and `ruby/setup-ruby` no longer provide on the newest runners                                                                                                                                                                                                                                                                                                       |
| e2e               | the preparation step, the suite through `e2e-stream --strict`, the scenario report as the job summary and the artifacts                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| packaging         | GoReleaser and syft from their pinned actions; see [Release outputs](#release-outputs); the snapshot release's `before` hooks (`licenses check`, `notices check`); also a second `goreleaser release --snapshot --clean --skip=before,sign,sbom` from a fresh clone at another path, whose checksums of the archives must be identical (`diff -u`); `packages build --smoke` (the Linux x64 packages installed offline and compared with the binary) and a second `packages build`, whose checksum files must be identical; `ruby -wc` of the cask and checks of the Scoop and winget manifests |
| repository-checks | `pnpm dm check` (must leave no change; it lints the workflows, `update-schemas.yml` included, with actionlint, whose unknown runner labels `.github/actionlint.yaml` declares), then a strict documentation build, `pnpm dm exec zensical -- build --strict`, that fails on any warning                                                                                                                                                                                                                                                                                                         |

The macOS, Windows and Linux arm64 runners build the host target with
`goreleaser build --snapshot --single-target` and run
`packages build --all-targets=false --smoke` on it, which installs the npm
packages, the wheel and the gem offline and compares each launcher with the
binary. Linux x64 covers the same in E37 and E41.

`pnpm dm check` skips zizmor, which datamitsu marks as opt-in; run it on the
workflows with `pnpm dm exec zizmor -- --offline .github/workflows`. In CI
(`CI=true`) datamitsu also runs tools it skips locally: lychee, knip,
bearer, trivy, grype and trufflehog (`CI=true pnpm dm lint` runs them
locally). osv-scanner is skipped everywhere through `.datamitsuignore`: the
version the datamitsu configuration pins cannot analyse code for the Go
version in `go.mod`; `osv-scanner.toml` keeps its reviewed exemptions for
when it returns.

`task verify` also builds the documentation with `--strict`, as does
`pnpm docs:build`: any warning fails the build.

## Which stages need the network

| Stage                                                               | Network                                                                                                                                                                         |
| ------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Unit, integration, fuzz                                             | none once `task tools` has run and the module cache is filled (the license check reads modules that only Windows links)                                                         |
| Tool installation (`task tools`)                                    | the Sourcemeta release download (checksum verified) and the Go module proxy for GoReleaser (checksum database verified)                                                         |
| `pnpm install`, `datamitsu init`, the first `pnpm dm exec syft`     | npm registry and the tool downloads datamitsu manages                                                                                                                           |
| E2E preparation                                                     | image pulls (Docker Hub), Python wheels for the validator image and the wheels' hatchling (PyPI), Go modules (`go mod download`)                                                |
| `packages build`                                                    | PyPI for the hash-pinned hatchling (uv caches it); with `--ruby docker`, Docker pulls the pinned Ruby image once if it is missing, and the container runs with `--network none` |
| `packages publish-plan`, `npm publish`, the PyPI upload, `gem push` | npmjs.org, PyPI and rubygems.org, only in the release workflow's publishing jobs                                                                                                |
| E2E scenarios                                                       | none beyond the local registries; a missing image or Go module fails the run instead of being downloaded                                                                        |
| `govulncheck`                                                       | Go vulnerability database                                                                                                                                                       |
| `licenses`/`notices` `generate`, `check`                            | the Go toolchain named in `go.mod` and modules missing from the module cache                                                                                                    |
| Weekly catalog update                                               | SchemaStore (`git ls-remote`, tarball), GHCR, the GitHub API (maintainer side only)                                                                                             |
