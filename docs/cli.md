# Command reference

```text
schepherd [global flags] <command> [flags] [arguments]
```

Standard output belongs to the command result. Progress and diagnostics go
to standard error only. No command prints banners, colors or update notices,
and nothing is sent anywhere except to the registries you configure.

## Global flags

| Flag                  | Environment            | Meaning                                                               |
| --------------------- | ---------------------- | --------------------------------------------------------------------- |
| `--config <file>`     | `SCHEPHERD_CONFIG`     | Local configuration file. Never discovered automatically.             |
| `--repository <repo>` | `SCHEPHERD_REPOSITORY` | `host[:port]/path` in lower case holding the schema set.              |
| `--catalog <digest>`  | `SCHEPHERD_CATALOG`    | `sha256:<hex>` of the catalog index.                                  |
| `--cache-dir <dir>`   | `SCHEPHERD_CACHE_DIR`  | Cache root (default: OS user cache dir + `/schepherd`).               |
| `--offline`           | `SCHEPHERD_OFFLINE`    | Forbid all network access by Schepherd itself.                        |
| `--workspace <dir>`   | `SCHEPHERD_WORKSPACE`  | Root for path matching (default: current directory).                  |
| `--timeout <dur>`     | `SCHEPHERD_TIMEOUT`    | Deadline for Schepherd's own registry and cache work (default `10m`). |
| `--quiet`             |                        | Suppress Schepherd's own progress and diagnostic lines on stderr.     |

`--timeout` covers every registry request and the wait for a cache lock. When
the deadline passes, the command fails with exit code 4 and a registry error
that says it timed out. It does not limit a consumer started by `run`; that is
`runner.timeout` (exit code 124).

`--quiet` also drops the `consumer exited with status N` line of `run`. Errors
are still printed, and a consumer's own standard output and standard error
pass through unchanged.

Environment values are parsed as described under
[Precedence](configuration.md#precedence). An invalid `SCHEPHERD_TIMEOUT` or
`SCHEPHERD_OFFLINE` is a usage error (exit code 2) even when the matching
flag is also given.

## Getting schemas

### `path <id>`

Prints the absolute path of the verified, materialized `schema.json`, followed
by a newline (`--null`: a NUL byte). For a local schema
([Local schemas](configuration.md#local-schemas)) it prints the absolute path
of the local file itself, which is checked but never copied. The file exists
when the command succeeds. On failure stdout is empty and the exit code is
non-zero.

```bash
schema_file="$(schepherd --config ./schepherd.toml path package)" || exit $?
"$VALIDATOR_BIN" validate "$schema_file" package.json
```

For a catalog schema, the path depends only on the schema manifest digest,
so it is stable across runs, mirrors and working directories. The cache can be deleted by you or
the OS at any time; use `export` for long-lived copies.

### `cat <id>`

Writes the schema JSON bytes (decompressed, exactly as materialized) to
stdout. For a local schema, the file's bytes as they are. Nothing else is
written to stdout.

### `export <id> <destination> [--force] [--json]`

Writes an independent copy of the verified schema to `destination`: a new
file, never a link to the cache, so deleting or cleaning the cache later does
not affect it. The file mode is `0644`.

When the schema artifact carries a notice (the attribution and license text
published with the schema, see [Notice layer](oci-format.md#notice-layer)),
`export` also writes it, verified by digest and size like the schema, to
`<destination>.NOTICE` (mode `0644`). Keep the two files together when you
vendor the schema: the license may require the notice. The notice file is
written first, so a complete schema file never appears without it. Local
schemas have no notice.

`export` gets the schema through the cache exactly like `path` and `cat`.
When the schema is not cached yet, it is fetched, verified and materialized
into the cache first; a corrupt cache entry is quarantined and fetched again.
The notice is not needed to materialize a schema, so `path`, `cat` and `run`
never fetch it: `export` reads it from the cache, or fetches and caches it
there on first use. Only a warm, intact cache is left unchanged. With
`--offline`, a missing or corrupt schema or notice fails with exit code 6
and nothing is written. A local schema is copied from its file under the
same no-clobber and atomic rules; nothing is fetched.

Without `--force`, an existing file (or symlink) at `destination` or at
`<destination>.NOTICE` is never overwritten, even when the schema has no
notice: the command fails with exit code 2 (`already exists; pass --force
to replace it`), checked before anything is fetched and again when each file
is put in place. If the schema file cannot be put in place, the notice file
this run created is removed again, unless another export has replaced it
since. Each file is first written next to its
destination and then hard-linked into place, so it appears complete and at
once. On filesystems that cannot create hard links (FAT, exFAT, some network
or FUSE mounts) the destination is created exclusively instead: an existing
or concurrently created file is still never overwritten, but another process
may see the file before it is completely written, and a partial file is
removed if writing fails.

With `--force` both files are replaced through an atomic rename; a symlink
is replaced, not followed. When the schema has no notice, a file or symlink
at `<destination>.NOTICE` left by an earlier export is removed, so it cannot
attribute the wrong schema. If either destination is a directory, the
command fails before changing anything; if the schema file cannot be put in
place after the new notice, the previous notice is restored.
The destination path is otherwise used as given: if its parent directory is
a symlink, the files are written into the directory the symlink points to.

Without `--json` nothing is printed on success. With `--json` it prints
`{id, origin, path, notice?}`: the schema ID, `catalog` or `local`, the
absolute path of the schema file and, when the schema has one, of its notice
file.

## Catalog and discovery

These commands fetch the catalog only (never schema artifacts); without
`[catalog]`, `list`, `patterns` and `resolve` use only the local schemas.

| Command                 | Output (`--json`)                                                                                                                                                                        |
| ----------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `catalog`               | The verified catalog document, byte for byte.                                                                                                                                            |
| `list`                  | `[{id, name, description?, dialect?, digest, origin: "catalog"}]`; a local schema is `{id, origin: "local", schemaPath, shadows}`.                                                       |
| `patterns`              | `[{id, fileMatch, artifact, origin: "catalog"}]` for catalog schemas that have `fileMatch`; a local schema with `file_match` is `{id, fileMatch, origin: "local", schemaPath, shadows}`. |
| `resolve --file <path>` | `{file, path, schema, origin, artifact}` for a catalog schema, `{file, path, schema, origin, schemaPath}` for a local one.                                                               |

Without `--json`, `catalog` prints a summary and `resolve` prints the ID.
`list` prints `id<TAB>name` for catalog entries and `id<TAB>schemaPath<TAB>local`
for local ones. `patterns` prints `id<TAB>pattern` lines, with a third column
`local` for local schemas. The marker reads `local, shadows catalog` when the
entry replaces a catalog ID. Items are in ID order, and a shadowing local
schema replaces the catalog entry in both commands.

`resolve` only matches paths; the file does not need to exist and is never
read. Local `[[mappings]]` take precedence over local `file_match`, which
takes precedence over catalog `fileMatch`. `origin` is `mapping`, `local` (a
local schema's `file_match`) or `catalog`. When the result is a local schema,
its file is checked like `path` does (exit code 2 if it is unusable).

### `pin <reference> [--json]`

Explicit, networked operation that resolves a tag once:

```bash
schepherd pin registry.example/org/schemas:catalog-latest --json
```

```json
{
  "repository": "registry.example/org/schemas",
  "tag": "catalog-latest",
  "digest": "sha256:…",
  "revision": "20260930.0300",
  "schemas": 412
}
```

Without `--json` it prints a `[catalog]` section with `repository` and
`digest` (and the revision as a comment), ready to paste into the
configuration file.

`digest` is the digest of the catalog index. `tag` is omitted when the
argument is already pinned by digest. It verifies the catalog it resolved
(the index, its metadata manifest and `catalog.json`, which must list exactly
the index's schema manifests) and changes nothing on disk. Other
commands refuse tags: `--catalog catalog-latest` is an error that points to
`pin`.

`pin` and `mirror` take their repositories from their arguments and do not
use the `[catalog]` section. `catalog.repository`, `catalog.digest`,
`--repository`, `--catalog`, `SCHEPHERD_REPOSITORY` and `SCHEPHERD_CATALOG`
are neither expanded nor validated for them, so a configuration with
`digest = "${SCHEPHERD_CATALOG}"` works for
`schepherd --config ./schepherd.toml pin <repository>:catalog-latest` before
`SCHEPHERD_CATALOG` exists. The `registries` settings (`plain_http`,
`ca_file`, `credentials_file`), the limits, `offline` and the timeout still
apply. A literal malformed value in the file, such as
`digest = "catalog-latest"`, is still a configuration error (exit code 2).
Every command that needs the pinned catalog (`path`, `cat`, `export`,
`catalog`, `list`, `patterns`, `resolve`, `run`) still fails with exit code 2
when the repository or the catalog digest is unset or empty, unless it uses
only local schemas ([Local schemas](configuration.md#local-schemas)).

## `mirror <source-repository>@<catalog-digest> <destination-repository> [--concurrency <n>] [--json]`

Copies the complete snapshot (the OCI graph of the catalog index: every
schema artifact, the catalog metadata, then the index itself) byte for byte
and tags it `catalog-<revision>`. The catalog digest stays the same.
`--concurrency` sets how many manifests and blobs are copied at once (default
4, 1–64). Like `pin`, `mirror` ignores the `[catalog]` section; source and
destination each use the `registries` entry that applies to them. Every blob
copied from the source is verified by digest and size on the way (a mismatch
exits with code 5 before any catalog tag is created), and an interrupted
mirror exits with code 130 and can be re-run. See [Mirroring](mirroring.md).

## `run [--schema <id>] [--ignore-unmatched] [--report <file>] -- <files...>`

Runs the configured consumer for the given files. Files are paths you pass;
Schepherd does not walk directories or expand globs. With `--schema`, every
file uses that schema. Otherwise each file is resolved like `resolve`;
unknown files and ambiguous matches are errors before any consumer starts.
Catalog schemas are materialized; local schemas are checked in place.
`--ignore-unmatched` skips files that match nothing or lie outside the
workspace (reported on stderr and in the report as `unmatched` or
`outside-workspace`) but never skips ambiguous ones. See [Runner](runner.md).

## Other commands

| Command                 | Purpose                                                                                      |
| ----------------------- | -------------------------------------------------------------------------------------------- |
| `config check [--json]` | Validate the configuration (and bases) and every local schema file without running anything. |
| `version [--json]`      | Print the version: `X.Y.Z[-prerelease]` for a release build, `dev` otherwise.                |
| `help [command]`        | Show help for any command (same as `--help`).                                                |

`version --json` prints `version`, `commit` and `commitDate` (when known),
`goVersion`, `release`, `catalogFormatVersions` and `configFormatVersions`;
see [Binary version](versioning.md#binary-version).

## Exit codes

| Code | Meaning                                                                                                                                                                                                                                                                                                                            |
| ---- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 0    | Success                                                                                                                                                                                                                                                                                                                            |
| 1    | Unexpected internal error                                                                                                                                                                                                                                                                                                          |
| 2    | Invalid arguments or configuration; unsupported format version (catalog `formatVersion`, `config_version`, artifact wire format such as `…schepherd.schema.v3`); a local schema file that is missing, unreadable, not a regular file, too large or not valid JSON; a `[schemas]` ID whose `path` no file of the extends chain sets |
| 3    | Unknown schema, no match, ambiguous match                                                                                                                                                                                                                                                                                          |
| 4    | Registry, authentication or network failure; Schepherd's own `--timeout` exceeded                                                                                                                                                                                                                                                  |
| 5    | Integrity failure: digest/size mismatch, invalid artifact (including a catalog index and `catalog.json` that disagree on the schema manifests), limit                                                                                                                                                                              |
| 6    | Offline cache miss or corrupt cache entry in offline mode                                                                                                                                                                                                                                                                          |
| 7    | The consumer could not be started: command not found or not executable, a `.bat` or `.cmd` file on Windows, unusable working directory                                                                                                                                                                                             |
| 124  | A consumer exceeded `runner.timeout`                                                                                                                                                                                                                                                                                               |
| 130  | Interrupted                                                                                                                                                                                                                                                                                                                        |

`run` returns the consumer's own exit status when a consumer ran and failed.
Those numbers can coincide with the codes above; stderr says which component
failed.
