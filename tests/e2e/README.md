# End-to-end suite

Black-box tests of the real `schepherd` and `schepherd-publisher` binaries
against real OCI registries started with Docker Compose. Nothing here imports
the product's `internal/` packages: the suite checks the documented behaviour
(docs/cli.md, docs/oci-format.md, docs/publishing.md), not the current
implementation. Test-only helpers live next to it: `testconsumer` (a fake
validator driven by `TC_*` variables), `credhelper` (installed as
`docker-credential-e2e`) and `regproxy` (a counting, fault-injecting reverse
proxy placed in front of a real registry).

## Prerequisites

- Linux. The suite mounts the binaries it builds for the host into Linux
  containers, copies the host's Sourcemeta binary into a Linux image and
  reads `/proc`, so on any other system it **fails** at start.
- A running Docker Engine with the Compose v2 and buildx plugins
  (`docker version`, `docker compose version`, `docker buildx version`).
  Without them the suite **fails** with an explanation; it never skips.
- Go as pinned in `go.mod`.
- The pinned Sourcemeta binary and GoReleaser in `.tools/bin`:
  `pnpm dm exec task -- tools` (the `test:e2e` task does this for you).
- npm on the host: E37 builds the tarballs with `packages build` of the
  release tool (`npm pack`) and publishes them to Verdaccio in the order of
  `packages publish-plan`, interrupted after two packages and then resumed,
  as the release workflow's publish-npm job does.
- uv on the host: E41 builds the wheels with `packages build` (hatchling
  through `uv build`, offline from the cache the preparation filled) and the
  gem in the pinned Ruby image (`--ruby docker`), and installs them into the
  pinned Python and Ruby images.
- The preparation step below, once per machine and after changing an image
  pin or `validators/`. It is the only step that uses the network.

## Preparation

```bash
pnpm test:e2e:install   # runs the test:e2e:prepare task
# or, without go-task:
go mod download && E2E_PREPARE=1 go test -tags e2e -count=1 -run '^$' -v ./tests/e2e
```

With `E2E_PREPARE=1` the test binary runs no test: it pulls every pinned
image of the run (the `compose.yaml` services of all profiles, `nodeImage`,
`bunImage`, `pythonImage`, `rubyImage`) and builds the validators image (pip downloads hash-pinned
wheels from PyPI), skipping what the local image store already has, and
prints one line per image (`present`, `pulled` or `built`). It then puts the
hash-pinned hatchling of `packaging/python/build-constraints.txt` into uv's
cache with `uv pip install`, from which E41 builds the wheels with
`UV_OFFLINE=1`. A failed
validators build leaves `validators-build.log` in
`tests/e2e/.artifacts/prepare` (or `E2E_ARTIFACTS_DIR`).

The scenarios download nothing. A run first checks that every one of those
images is present and otherwise fails before starting anything, naming the
missing images and this step. Compose starts services with `--pull never`
and `dockerRun` runs containers with `--pull never`, so an image that
disappears during a run fails the scenario instead of being fetched. The Go
commands the harness runs on the host (the builds of the binaries,
`hostTool` with `go run ./tools/release` or GoReleaser) get `GOPROXY=off`:
they use the module cache `go mod download` filled, and a missing module
fails the run instead of being downloaded.

## Running

```bash
pnpm test:e2e                                           # preparation, then everything, with a report
go test -tags e2e -count=1 -timeout 60m ./tests/e2e/... # the same without the report (prepare first)
go test -tags e2e -count=1 -run TestE01 -v ./tests/e2e/...
pnpm test:e2e:perf                                      # the TestPerf_* measurements only
```

`task test:e2e` (`pnpm test:e2e`) runs `test:e2e:prepare` as its first,
separately reported step, then the suite with `go test -json` piped into
`go run ./tools/release e2e-stream`, which prints the test output as it
arrives and writes to `E2E_ARTIFACTS_DIR` (default
`tests/e2e/.artifacts/latest`):

- `e2e.json`: the raw `go test -json` stream, written as it arrives;
- `e2e-report.md`: written when the stream ends, **also when tests fail or
  the run is interrupted**. It lists every required scenario
  (`RequiredScenarios` in `tools/release/internal/e2ereport`) with PASS, FAIL
  or NOT RUN (NOT RUN when no test of the ID ran, for example after a `-run`
  filter, a rename or a deletion, or when its tests were skipped), then every
  test and subtest.

The task fails when any test failed and also when any required scenario did
not pass (`--strict`). `go run ./tools/release e2e-report --in e2e.json --out
report.md [--strict]` renders the same report from a saved stream.

`E2E_ARTIFACTS_DIR` chooses where logs and reports go (default
`tests/e2e/.artifacts/<project>` for a plain `go test`, gitignored). A
relative value is resolved against the repository root. `E2E_PERF=1` turns
the `TestPerf_*` measurements on (any `strconv.ParseBool` value; an invalid
one fails the run at start); `task test:e2e:perf` sets it, runs only
`TestPerf_*` and keeps the results in `E2E_ARTIFACTS_DIR/perf/`:
`TestPerf_SyntheticCatalog.json` (every step with its command, duration,
exit code, stdout size, the client's cache size afterwards and, per registry
proxy, requests by class, bytes sent, bytes received and blob bytes
received) and `TestPerf_SyntheticCatalog.md` (the same as a table).

## What a run does

1. Checks the platform, Docker, Compose and buildx, finds the repository
   root and the Sourcemeta binary, and checks that every image of the run is
   in the local image store (see Preparation).
2. Picks a unique Compose project `schepherd-e2e-<pid>-<random>` and a work
   directory in `$TMPDIR`, and starts a **reaper** (see below).
3. Builds `schepherd`, `schepherd-publisher`, `testconsumer` and
   `docker-credential-e2e` with `CGO_ENABLED=0 -trimpath`.
4. Generates a test CA, one server certificate (SAN `localhost`, `127.0.0.1`,
   `registry-auth`, `registry-auth2`) and two htpasswd files (bcrypt) with
   different random users.
5. `docker compose -p <project> -f compose.yaml --profile auth up --detach --wait --pull never`,
   discovers every published port with `docker compose port`, and polls
   `/v2/` every 200 ms for up to 90 s (200 for plain registries, 401 for the
   auth ones). No service has a healthcheck (they are disabled in
   `compose.yaml`), so `--wait` only waits for the containers to run and
   readiness never depends on a tool inside an image; verdaccio is probed
   the same way on `/-/ping`.
6. Starts one shared `regproxy` in front of `registry-source` and one in
   front of `registry-mirror`.
7. Runs the tests. On failure (or SIGINT/SIGTERM) it writes
   `compose.log` and `compose-ps.txt` to the artifacts directory **before**
   cleanup.
8. Always stops the run's docker commands that are still running, removes run
   containers, then
   `docker compose -p <project> down --volumes --remove-orphans`, and verifies
   that no container, anonymous volume or network of the project is left
   (with a second pass if something was left); leftovers fail the run. It
   never prunes and never touches other projects.

Every docker command the harness starts for the project (compose commands,
`dockerRun`) carries `SCHEPHERD_E2E_OWNER=<project>` in its environment,
and the docker CLI passes it on to the compose plugin. Before removing the
project, teardown and the reaper kill the processes of the current user that
carry this entry and wait until they are gone, so an orphaned
`docker compose up` cannot recreate a container or the network afterwards.

The reaper is the test binary re-executed in its own session, reading a pipe
held by the test process. If the test process dies without teardown (a
`-timeout` panic, SIGKILL, a crash), the pipe closes and the reaper stops the
orphaned docker commands, dumps the logs, removes the project and the work
directory, and writes `reaper.log` to the artifacts directory. After a
teardown triggered by SIGINT/SIGTERM, test goroutines may still start docker
commands until the process exits, so the reaper runs a second, quiet pass
once the process is gone (it writes `reaper.log` only if that pass fails).

## Infrastructure

| Service                           | Profile | Purpose                                                          |
| --------------------------------- | ------- | ---------------------------------------------------------------- |
| `registry-source`                 | —       | Plain HTTP registry, publishing target (shared proxy in front)   |
| `registry-mirror`                 | —       | Plain HTTP registry, mirror destination (shared proxy in front)  |
| `registry-auth`, `registry-auth2` | `auth`  | TLS (test CA) + htpasswd, different users; auth/mirror scenarios |
| `verdaccio`                       | `npm`   | npm registry without uplinks; started on demand by `npmRegistry` |

All registries are `distribution/distribution:3.1.2` pinned by digest, with relative upload
URLs, publishing an ephemeral port on 127.0.0.1. A port changes whenever a
container restarts. `validators/` is built by the preparation step into the
local image
`schepherd-e2e-validators:<sha256 of Dockerfile, requirements.txt, .dockerignore
and the Sourcemeta binary>` (reused when present, never deleted):
check-jsonschema 0.38.2 (hash-pinned `requirements.txt`, regenerate with
`uv pip compile requirements.in --generate-hashes --python-version 3.14 --python-platform x86_64-manylinux_2_28 -o requirements.txt`,
the command in the file's header, for the image's `python:3.14-slim`)
and the Sourcemeta `jsonschema` CLI in `/usr/local/bin`.

## Cleanup and troubleshooting

- **Leftovers after a crash the reaper could not handle** (for example the
  machine rebooted). Replace `<project>` with the name printed at the start of
  the run:

  ```bash
  docker ps -aq --filter label=com.docker.compose.project=<project> | xargs -r docker rm -f -v
  docker ps -aq --filter label=com.ovineko.schepherd.e2e.project=<project> | xargs -r docker rm -f -v
  docker network rm <project>_default
  ```

- **`docker compose` complains about `SCHEPHERD_E2E_*_DIR`**: compose.yaml is
  meant to be started by the harness, which generates those directories.
- **A command that talks to a proxy that is set down takes about 7 s**: the
  client retries 5xx responses and timeouts before exiting with code 4. A
  stopped registry reached directly refuses connections and fails at once.
  Keep such steps few.
- **Docker from snap, or a daemon with user-namespace remapping**: the daemon
  must be able to read the work directory in `$TMPDIR` (certificates and
  htpasswd files are world-readable on purpose). Point `TMPDIR` elsewhere if
  it cannot.
- **A run fails at start with "image(s) the scenarios use are not in the
  local image store"**: run the preparation step. It is needed again after
  an image pin, `validators/` or the Sourcemeta binary changed.
- **Validators image build fails** (preparation): the log is saved as
  `validators-build.log` in the artifacts directory. The image is built with
  `docker buildx build --load`, so it also reaches the local image store
  when the selected builder uses the docker-container driver
  (`docker buildx ls` shows which builder is selected).

## Writing scenarios

- One test per scenario, `Test<ID>_<Name>` (for example `TestE07_OneSchemaChanged`);
  scenarios are grouped by topic, one file per group (`artifacts_test.go`,
  `mirror_test.go`, ...); subtests are fine. Files start with `//go:build e2e`.
  A scenario may have more than one test with its ID (for example E35,
  "Catalog revisions": `TestE35_CatalogRevisions`, `TestE35_RevisionGrammar`,
  `TestE35_RemovedUpstream`, `TestE35_ExcludedByRule` and
  `TestE35_FinishFromState` in `revision_test.go`); it passes in the report
  only when all of them pass. The IDs are the matrix in
  `tools/release/internal/e2ereport` (`RequiredScenarios`); its unit test
  fails when a required ID has no test here or a test uses an ID outside
  the matrix.
- Registries are shared by the whole run: always work in `repoPath(t)` (or
  `repoPath(t, "other")` for a second repository).
- Tests that stop services, set a shared proxy down, add faults to it or
  count its requests must not call `t.Parallel`. Prefer `newProxy(t, reg)`,
  a private proxy whose records and faults belong to the test alone.
- Use the binaries only through `cli`, `publisher` or `startBin`; they run in
  an isolated sandbox and never see the developer's HOME, Docker
  credentials or `SCHEPHERD_*` variables.
- Run host tools such as `go run ./tools/release ...` or
  `.tools/bin/goreleaser` through `hostTool`, which uses the developer's
  environment and the repository root.
- Read the environment only through `env_test.go` (for example
  `perfEnabled()`), never with `os.Getenv` in a scenario.
- Never skip; English only; no comments that restate code.

## Helper API

The suite state is the package variable `suite`: `suite.source`,
`suite.mirror`, `suite.auth`, `suite.auth2` (`*registry`),
`suite.sourceProxy`, `suite.mirrorProxy` (`*proxy`), `suite.pki.CAFile`,
`suite.bin`, `suite.jsonschema` (pinned Sourcemeta binary).

### Running binaries (harness_test.go)

| Helper                                                      | Purpose                                                                                                                                                                        |
| ----------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `cli(t, runOpts{}, args...) result`                         | Run `schepherd`; any exit code is a normal result, a hang (default 5 min) fails the test.                                                                                      |
| `publisher(t, runOpts{}, args...) result`                   | Run `schepherd-publisher`.                                                                                                                                                     |
| `runBin(t, name, opts, args...)`                            | Run `testconsumer`, `docker-credential-e2e`, ...                                                                                                                               |
| `startBin(t, name, opts, args...) *process`                 | Start without waiting: `PID()`, `Signal(sig)`, `Wait() result` (safe from any goroutine); killed at test end.                                                                  |
| `binPath(name)`                                             | Absolute path of a built binary.                                                                                                                                               |
| `hostTool(t, runOpts{}, name, args...) result`              | Run a host tool with the developer's environment plus `GOPROXY=off` (`Env` on top), default directory the repository root; `name` is on the host PATH or relative to the root. |
| `perfEnabled()`                                             | Whether `E2E_PERF` asked for the `TestPerf_*` measurements.                                                                                                                    |
| `result{Args, Stdout, Stderr, Code, Signal, Duration, Err}` | `.ok(t)`, `.wantCode(t, n)`, `String()` for failure messages.                                                                                                                  |
| `runOpts{Sandbox, Dir, Env, Stdin, Timeout}`                | `Env` entries override the sandbox environment; `Stdin` nil is /dev/null.                                                                                                      |
| `sandboxOf(t)`, `newSandbox(t)`                             | Isolated `Home`, `CacheDir` (schepherd's default cache), `DockerConfig`, `Workspace`, `Tmp`; one per test by default.                                                          |

The sandbox PATH is `<run bin dir>:/usr/local/bin:/usr/bin:/bin`, so
`testconsumer` and `docker-credential-e2e` resolve by name.

### Configuration

| Helper                                                        | Purpose                                                                                                                                                                                                                                  |
| ------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `clientConfig{Repository, Catalog, Registries, Extra}.TOML()` | A `config_version = 1` file with `[catalog]`, a `[registries."host"]` table for every suite registry and live proxy (plain_http, or ca_file for TLS) and `Extra` appended verbatim. `Registries[host]` replaces a table; `nil` drops it. |
| `writeConfig(t, ws, toml) string`                             | Write `ws/schepherd.toml`; pass it with `--config`.                                                                                                                                                                                      |
| `registrySettings{PlainHTTP, CAFile, CredentialsFile}`        | One registries table.                                                                                                                                                                                                                    |
| `writeDockerConfig(t, dir, map[host]credential) string`       | `dir/config.json` with static auths (a `credentials_file`, or `sandbox.DockerConfig`).                                                                                                                                                   |
| `writeCredsStoreConfig(t, dir, "e2e")`                        | `dir/config.json` with `credsStore`; set `E2E_CREDHELPER_DB` / `E2E_CREDHELPER_LOG` in `runOpts.Env`.                                                                                                                                    |
| `publisherRegistryConfig(t) string`                           | The publisher's `--registry-config` for the current addresses, with credentials for the auth registries.                                                                                                                                 |
| `tomlString(s)`                                               | Quote a TOML string.                                                                                                                                                                                                                     |

### Publishing and fixtures

| Helper                                               | Purpose                                                                                                  |
| ---------------------------------------------------- | -------------------------------------------------------------------------------------------------------- |
| `newSet(t, "set-basic")`                             | Copy `fixtures/<name>` to a temp dir (modify the copy freely).                                           |
| `prepareSet(t, set, extra...) string`                | `prepare --source set/source.toml --jsonschema <pinned> --json`; returns the prepared directory.         |
| `publishPrepared(t, repo, prepared, extra...)`       | `publish --registry-config ... --json` (must succeed); returns `publishResult` (`Raw`, `Prepared` kept). |
| `publishSet(t, repo, set, extra...)`                 | Both steps. Typical extras: `--now YYYYMMDD.HHMM`, `--state`/`--state-out <file>`, `--update-latest`.    |
| `readPrepared(t, dir)`, `preparedSchema(t, dir, id)` | `prepared.json` and the exact prepared bytes of a schema.                                                |
| `repoPath(t, name...)`                               | `e2e/<test name>/<name or "schemas">`; combine with `reg.Repo(path)` or `proxy.Repo(path)`.              |
| `fixture(parts...)`, `instance(id, file)`            | Paths below `fixtures/`.                                                                                 |
| `stdinFixtures`, `writeStdinFixtures(t, dir)`        | Byte-exact stdin documents (CRLF, comments, Unicode, no final newline).                                  |
| `newDepServer(t)`, `newDepsSet(t, dep.Host())`       | Dependency server for the deps fixture (`Set(path, data)`, `Hits(path)`) and the rendered set.           |

### Registries and proxies (registry_test.go)

| Helper                                                        | Purpose                                                                                                                                                                                                                       |
| ------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `reg.Host()`, `reg.Repo(path)`                                | Direct address `127.0.0.1:<port>` (read again after a restart).                                                                                                                                                               |
| `reg.Manifest(t, repo, ref) fetchedManifest`                  | GET a manifest by tag or digest, digest-verified, decoded (`Body`, `Digest`, `ContentType`, `Manifest`).                                                                                                                      |
| `reg.Blob(t, repo, digest)`                                   | GET a blob, digest-verified.                                                                                                                                                                                                  |
| `reg.ManifestStatus`, `reg.BlobStatus`                        | HEAD status codes.                                                                                                                                                                                                            |
| `reg.Tags(t, repo)`                                           | Sorted tags; nil when the repository does not exist.                                                                                                                                                                          |
| `reg.TLS()`, `reg.User()`                                     | Auth registries: TLS with the test CA and their htpasswd `credential`.                                                                                                                                                        |
| `suite.sourceProxy`, `suite.mirrorProxy`, `newProxy(t, reg)`  | `Host()`, `Repo(path)`, `Stats(t)`, `Records(t)`, `Reset(t)`, `AddFault(t, regproxy.Fault)`, `ClearFaults()`, `SetDown(t, bool)`; faults and down are undone at test end. A proxy keeps its address across registry restarts. |
| `pushRaw(t, reg, repo, rawArtifact{...}, tags...)`            | Push a hand-crafted (possibly hostile) artifact with ORAS: layers, declared sizes, `Edit` for arbitrary manifest members.                                                                                                     |
| `schemaLayer(mediaType, payload, content)`, `gzipBytes(data)` | Payload layer with the content annotations and the title `payloadTitle(mediaType)` (`schema.json`, or `schema.json.gz` for gzip) that the publisher uses.                                                                     |
| `pushRawBlob`, `pushRawManifest`, `pushRawCatalog`            | Lower-level pushes; `pushRawCatalog` accepts any bytes as the catalog document.                                                                                                                                               |
| `catalogDoc`, `catalogEntry`, `descriptorOf(desc)`            | Decode published catalogs or build raw ones.                                                                                                                                                                                  |
| media type constants                                          | `mediaTypeSchemaJSON`, `mediaTypeSchemaGzip`, `mediaTypeNotice`, `mediaTypeCatalog`, `artifactTypeSchema`, ...                                                                                                                |

### Docker (docker_test.go)

| Helper                                           | Purpose                                                                                                                                                                                                            |
| ------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `stopService(t, name)`, `startService(t, name)`  | `docker compose stop/start --wait`; start rediscovers the new host port (`reg.Host()`, `npmRegistry(t)`) and waits for the HTTP readiness probe; a stopped service is restarted at test end.                       |
| `serviceLogs(t, name)`                           | Logs of one service (for example to check which user a registry saw).                                                                                                                                              |
| `dockerRunValidators(t, mounts, args...)`        | Run in the validators image with `--network none --user <uid>:<gid> --read-only`.                                                                                                                                  |
| `dockerRun(t, image, dockerOpts{...}, args...)`  | Any prepared image (`--pull never`); `Network: composeNetwork()` reaches services by name.                                                                                                                         |
| `mount{Host, Container, Writable}`, `binMount()` | Bind mounts; `binMount()` puts the run's static binaries at `/e2e/bin`.                                                                                                                                            |
| `npmRegistry(t)`                                 | Start verdaccio on first use; its current host URL. Containers use `http://verdaccio:4873`.                                                                                                                        |
| `nodeImage`, `bunImage`, `validatorsImage(t)`    | Images for `dockerRun`: node 26.10.0 (npm, npx) and bun 1.4.2, pinned by digest, and the validators image. A new image must also be listed in `suiteImages` (prepare_test.go).                                     |
| `pythonImage`, `rubyImage`                       | The PyPI and RubyGems images of E41, pinned by digest: the validators image's base (`TestHarness_WrapperImages` keeps them equal) and `pins.RubyImage` (`tools/pins/ruby.go`), the Ruby image of the release tool. |

### Files and processes

`writeFile`, `writeFiles`, `readFile`, `copyTree`, `fileTree` (assert that
nothing appeared somewhere), `digestOf`, `sha256Hex`, `decodeJSON[T]`,
`eventually(t, timeout, what, cond)`, `processAlive(pid)`,
`waitGone(t, pid, timeout)`.

## Fixtures

- `set-basic/`: local source (`source.toml`, `licenses.toml` allowing
  `https://schemas.example.com/e2e/` as MIT with a notice) with `alpha`
  (draft-07; `alpha.json`, `**/.github/workflows/*.yml`), `beta` (2020-12;
  `config/**/*.toml`, `beta.yaml`), `gamma` (large, stored gzip; `gamma.json`),
  `delta` (2019-09, no fileMatch), `dup1`/`dup2` (both `compose.yml`).
- `set-basic-v2/`: identical except `schemas/beta.json`.
- `deps/`: template with `{{DEP_HOST}}`; `deps-root` references
  `http://{{DEP_HOST}}/dep.json` (`served/dep.json`, changed version
  `served/dep-v2.json`), `deps-plain` has no references. The source allows
  plain HTTP and the dependency server's host:port.
- `instances/<id>/{valid,invalid}.{json,yaml,toml}`: checked by
  `TestHarness_Validators` with both validators.
- `local/`: a repository with its own schemas for E40 (local schemas):
  `schemas/service.schema.json` takes its port definition from the sibling
  `schemas/common.schema.json` through a relative `$ref`,
  `schemas/alpha-override.schema.json` replaces set-basic's catalog schema
  `alpha`, and `services/*.json` and `owned/alpha.json` are the instances.
  `TestE40_LocalSchemas` names them in `[schemas]` of a configuration in
  `tools/`, so the paths are relative to that directory, and runs the real
  validator offline in the validators container without a cache.
- `examples/`: the `company-config` schema that `TestE31_ShippedExamples`
  adds to set-basic for the `[[mappings]]` of `examples/schepherd.toml`, with
  a valid and an invalid instance. That scenario runs the files of the
  repository's `examples/` directory unchanged.

The `TestHarness_*` tests check the fixtures, the helpers and the
infrastructure; run them after changing any of these.
