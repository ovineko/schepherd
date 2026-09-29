# Schepherd

Schepherd delivers JSON Schemas from a pinned catalog in an OCI registry to
verified local files. It downloads only the schemas you use, verifies each
one by digest and size, works offline from its cache, mirrors
complete snapshots into other registries without changing their digest, and
runs the validator of your choice when you ask it to. It never validates
documents itself and never contacts upstream schema URLs at runtime.

The name blends "schema" and "shepherd": it keeps a flock of JSON Schemas
pinned, cached and portable across registries.

## Install

npm, PyPI and RubyGems install a small launcher that runs the native binary
for your platform; Homebrew, Scoop and winget install the binary itself:

```bash
npm install --save-dev @ovineko/schepherd   # npm
pipx install schepherd                      # PyPI (or uv tool install, pip install)
gem install schepherd                       # RubyGems
brew install ovineko/tap/schepherd          # Homebrew (macOS, Linux)
scoop bucket add ovineko https://github.com/ovineko/scoop-bucket
scoop install schepherd                     # Scoop (Windows)
winget install ovineko.schepherd            # winget (Windows)
```

Release archives for Linux, macOS and Windows (x64 and arm64) and their
checksums are on the
[releases page](https://github.com/ovineko/schepherd/releases). With Go:

```bash
go install github.com/ovineko/schepherd/cmd/schepherd@latest
```

[Installation](docs/installation.md) covers every channel
([npm](docs/installation.md#npm), [PyPI](docs/installation.md#pypi),
[RubyGems](docs/installation.md#rubygems),
[Homebrew](docs/installation.md#homebrew), [Scoop](docs/installation.md#scoop),
[winget](docs/installation.md#winget)), the supported platforms,
[verifying the signatures and attestations of a release](docs/installation.md#verify-a-release)
and building from source. The binary needs no Docker,
no ORAS CLI and no particular validator.

## Usage

Pin a catalog once, then ask for schemas by ID:

```bash
schepherd pin ghcr.io/ovineko/schepherd-schemas:catalog-latest     # prints the repository and the catalog digest

export SCHEPHERD_REPOSITORY=ghcr.io/ovineko/schepherd-schemas
export SCHEPHERD_CATALOG=sha256:<catalog index digest>  # or paste pin's [catalog] section into schepherd.toml

schema_file="$(schepherd path package)" || exit $?
"$VALIDATOR_BIN" validate "$schema_file" package.json
```

`schepherd cat <id>` prints a schema, `schepherd export <id> <file>` vendors a
copy, `schepherd resolve --file <path>` finds the schema for a file, and
`schepherd mirror` copies a snapshot to another registry. Schemas of your own
repository can be declared as
[local schemas](docs/configuration.md#local-schemas) and are used in place.

The catalog at `ghcr.io/ovineko/schepherd-schemas` is prepared from
[SchemaStore](https://www.schemastore.org/) and updated by a weekly job, but it
is not all of SchemaStore: a schema is published only when its license allows
redistribution and it can be prepared as a valid, self-contained schema. See
[Coverage of the SchemaStore import](docs/publishing.md#coverage-of-the-schemastore-import)
and [Catalog automation](docs/automation.md).

## Documentation

The documentation of the latest release is published at
<https://schepherd.ovineko.com>; its source is [`docs/`](docs/index.md).
Start with [configuration](docs/configuration.md), the
[command reference](docs/cli.md) and the
[security and trust model](docs/security.md). [Versioning](docs/versioning.md)
explains the two release trains: the client (SemVer tags `vX.Y.Z`) and the
schema catalog (time-based revisions `catalog-YYYYMMDD.HHMM`).

## Contributing and security

See [CONTRIBUTING.md](CONTRIBUTING.md). Report vulnerabilities privately as
described in [SECURITY.md](SECURITY.md).

## License

Schepherd is released under the MIT license (see [LICENSE](LICENSE)). Schemas
distributed through Schepherd keep their own licenses, recorded in each
catalog entry and artifact; see [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
