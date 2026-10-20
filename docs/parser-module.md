# Structured validation parser module

Schepherd owns its Rust/WASM parser in `parsers/`. It is separate from the Go
client: `pnpm build`, `pnpm test` and ordinary Go tests do not require Rust.
Parser builds require the toolchain pinned in `parsers/rust-toolchain.toml`, its
`wasm32-unknown-unknown` target, Cargo and Node.js. Cargo installs the selected
Rust toolchain through rustup when needed; dependency downloads follow the
committed `Cargo.lock`, never an unlocked resolution.

## Local development

```bash
pnpm parsers:test
pnpm parsers:build
pnpm parsers:check
pnpm parsers:host
```

The build helper uses an isolated Cargo target directory, disables incremental
compilation, overrides inherited compiler flags and remaps the checkout, Cargo
home, Rust sysroot and target directory. The resulting artifact is retained at
`dist/parsers/schepherd-parsers.wasm`; only the intermediate target directory is
deleted. `check` executes the actual module in Node's WebAssembly engine, checks
ABI 2, capability schema 3, all four dispatch keys, validator findings and
Schepherd file attribution. `host` additionally loads the module through the
installed Datamitsu host and requires a classified diagnostic from an early
Schepherd failure.

These package commands call the corresponding `parsers:*` Taskfile tasks. For
an explicit version or output directory use the release helper directly:

```bash
go run ./tools/release parsers build --version 0.1.0 --dir dist/parsers
go run ./tools/release parsers check --version 0.1.0 --dir dist/parsers
go run ./tools/release parsers metadata --version 0.1.0 --dir dist/parsers
```

`metadata` does not rebuild: it validates the module's embedded version and
writes metadata for the exact existing bytes. Local commands default to
`0.0.0-dev`; an explicitly versioned build must be checked with the same version.

## Consumer contract

The module exports `memory`, `alloc`, `dealloc`, `parse`, `describe` and `reset`.
It returns ABI 2 responses and capability schema 3. Its dispatch keys are
`schepherd`, `sourcemeta`, `check_jsonschema` and `ajv`. The `schepherd` parser
consumes `schepherd run --json` reports and attributes each task's diagnostics
to its original input file, rather than a converted document or cached schema.
Standalone validator keys consume their respective machine-readable output;
their caller supplies the schema and input arguments.

The output directory contains:

| File                         | Purpose                                                                                          |
| ---------------------------- | ------------------------------------------------------------------------------------------------ |
| `schepherd-parsers.wasm`     | The actual platform-independent module                                                           |
| `schepherd-parsers.json`     | Version, ABI, capability schema, module SHA-256, size, release asset URL and OCI manifest digest |
| `schepherd-parsers.oci.json` | Exact OCI manifest bytes pinned by metadata                                                      |
| `SHA256SUMS`                 | SHA-256 of the other three files                                                                 |

`schepherd-parsers.json` is generated from real bytes, not a committed release
pin. Do not use a development artifact's release URL or OCI reference as if it
were already published. A consuming package adopts a release only after its
artifact has actually been published and checked.

## Release ordering and OCI integrity

GoReleaser's before hook builds the module at `.tools/parsers` using the release
or snapshot version. That directory survives GoReleaser's cleanup of `dist`.
The WASM, metadata and OCI manifest are extra release assets and are covered by
the signed release checksum file. The immutable GitHub release is published
only after these files have been built and their ABI has been exercised.

The OCI manifest is deterministic and computed before publication. It is an OCI
1.1 manifest of artifact type `application/vnd.datamitsu.parsers.v1+wasm`, with
an empty JSON config and exactly one uncompressed `application/wasm` layer.
The layer digest is the module's SHA-256. Thus the release metadata already
contains the exact future OCI manifest digest without inventing a registry
response or modifying an immutable release afterward.

After GitHub release publication, the same protected `release` job authenticates
to GHCR using its existing GitHub token and runs:

```bash
go run ./tools/release parsers publish --version 0.1.0 --dir .tools/parsers
```

This rechecks metadata, checksums and actual WASM execution, pushes the exact
recorded blobs and manifest to `ghcr.io/ovineko/schepherd-parsers`, then binds
`v0.1.0` without overwriting a tag that points elsewhere. It needs no new secret
or publication toggle.

Before signing, `parsers verify-published` validates the local generated metadata
against the module, then retrieves the immutable manifest by digest and verifies
its actual bytes. It requires the expected artifact and empty-config types,
exactly one uncompressed WASM layer, and the release metadata's layer size and
SHA-256. Both the config blob and module are retrieved and verified. The helper
uses a fresh explicit empty credential store: neither an earlier Docker login,
a credential helper nor `GITHUB_TOKEN` can make a private package pass. Public
registries may issue an anonymous pull token; no user credentials are supplied.

```bash
go run ./tools/release parsers verify-published --version 0.1.0 --dir .tools/parsers
```

The protected job then signs the same `ref@digest` keylessly with cosign and
attests that registry subject by name and digest, storing provenance in GitHub
and alongside the artifact in GHCR. File provenance and signed GitHub release
checksums remain separate and cover the release assets themselves.

A failure in the OCI push, anonymous retrieval, signing or attestation fails the
job even though the immutable GitHub release already exists. A newly created
GHCR package must be made public before the anonymous check can pass. Resume
with the release tooling from the original tagged commit and the exact signed
release files, not by overwriting published tags or regenerating an existing
release. `publish` accepts identical content but refuses a `v0.1.0` tag already
bound to another digest. Re-run the anonymous check, `cosign sign --yes` on the
verified metadata's `oci.ref@oci.digest`, and registry-subject provenance in the
protected release environment; publication alone is not a completed recovery.
Consumers may use the versioned GitHub asset URL and mandatory module hash
while OCI publication is repaired. See [Versioning](versioning.md) for the
immutable GitHub release recovery rules.

CI separately runs native Rust tests, a locked release build, ABI checks and the
actual host smoke, and compares artifacts built at different checkout paths.
The packaging job includes parser assets when rehearsing snapshot releases.
The mandatory frontend job runs AJV tests with frozen pnpm workspace dependencies
and Python frontend tests with `uv run --locked` and the committed `uv.lock`.
Python is pinned in `tools/check-jsonschema/.python-version`; CI provisions that
runtime and uv 0.12.19 without a setup cache. To run both suites locally:

```bash
pnpm install --frozen-lockfile
pnpm dm exec task -- test:frontends
```

The frontend task also runs in `task verify` and has no test-result cache.
