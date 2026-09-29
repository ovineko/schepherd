# Configuration

Schepherd reads one local TOML file (format `config_version = 1`). The file is
a **trusted, executable contract**: its `[runner]` section decides which
program runs with which arguments. Schepherd therefore never discovers a
configuration file on its own. It loads one only when you pass
`--config <file>` or set `SCHEPHERD_CONFIG=<file>`. Nothing fetched from a
registry can add commands, environment variables or configuration.

The JSON Schema of the file is [`api/config.schema.json`](https://github.com/ovineko/schepherd/blob/main/api/config.schema.json);
`schepherd config check` validates a file (including every `extends` base)
without running anything.

## Example

```toml
config_version = 1
extends = ["./base.schepherd.toml"]
workspace = "."
offline = false

[catalog]
repository = "${SCHEPHERD_REPOSITORY}"
digest = "${SCHEPHERD_CATALOG}"

[runner]
mode = "batch"
command = "${JSONSCHEMA_BIN}"
args = ["validate", "{schema}", "{files...}"]
cwd = "{workspace}"
timeout = "60s"
jobs = 1
fail_fast = false
inherit_env = true

[runner.env]
NO_COLOR = "1"
SCHEMA_ID = "{schema-id}"

[[mappings]]
file_match = ["config/company.json"]
schema = "company-config"
```

Runnable, tested versions of these files live in
[`examples/`](https://github.com/ovineko/schepherd/tree/main/examples) (see
[Tested validator configurations](integration.md#tested-validator-configurations)).
To run them, set `SCHEPHERD_REPOSITORY`, `SCHEPHERD_CATALOG` (a catalog
index digest from `schepherd pin`) and the validator variable
(`JSONSCHEMA_BIN` or `CHECK_JSONSCHEMA_BIN`), because every `${NAME}` must be
set.

## Reference

| Key                                   | Type             | Default                                   | Notes                                                                                                                                                             |
| ------------------------------------- | ---------------- | ----------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `config_version`                      | integer          | required                                  | Must be `1` in every file, including bases.                                                                                                                       |
| `extends`                             | array of strings | `[]`                                      | Local TOML files, relative to the declaring file.                                                                                                                 |
| `workspace`                           | path             | current directory                         | Root for `fileMatch` matching and `{workspace}`; relative to the file that sets it.                                                                               |
| `offline`                             | boolean          | `false`                                   | Same as `--offline`.                                                                                                                                              |
| `cache_dir`                           | path             | OS user cache dir + `/schepherd`          | Same as `--cache-dir`.                                                                                                                                            |
| `catalog.repository`                  | string           | none                                      | `host[:port]/path` in lower case, no tag or digest. Not used by `pin` and `mirror`.                                                                               |
| `catalog.digest`                      | string           | none                                      | `sha256:<hex>` of the catalog index. Tags are rejected. Not used by `pin` and `mirror`.                                                                           |
| `limits.max_manifest_bytes`           | integer          | 4194304                                   | See [OCI format](oci-format.md#limits).                                                                                                                           |
| `limits.max_catalog_bytes`            | integer          | 33554432                                  |                                                                                                                                                                   |
| `limits.max_catalog_entries`          | integer          | 20000                                     |                                                                                                                                                                   |
| `limits.max_payload_bytes`            | integer          | 67108864                                  |                                                                                                                                                                   |
| `limits.max_schema_bytes`             | integer          | 67108864                                  |                                                                                                                                                                   |
| `registries."<key>".plain_http`       | boolean          | `false`                                   | Opt in to HTTP for exactly the repositories this entry applies to. `<key>` is `host[:port]` or `host[:port]/path`; see [Registries](#registries-and-credentials). |
| `registries."<key>".ca_file`          | path             | system roots                              | Extra PEM CA bundle trusted for the repositories this entry applies to.                                                                                           |
| `registries."<key>".credentials_file` | path             | Docker config discovery                   | Docker-format `config.json` used only for the repositories this entry applies to.                                                                                 |
| `runner.mode`                         | string           | `batch`                                   | `batch`, `per-file` or `stdin`.                                                                                                                                   |
| `runner.command`                      | string           | required for `run`                        | Executable; see [Executable resolution](#executable-resolution). Must not expand to an empty string.                                                              |
| `runner.args`                         | array of strings | `[]`                                      | Argument templates.                                                                                                                                               |
| `runner.cwd`                          | path             | `{workspace}`                             | Working directory of the consumer. Must not expand to an empty string.                                                                                            |
| `runner.timeout`                      | duration string  | `5m`                                      | Per consumer process; exceeding it yields exit code 124.                                                                                                          |
| `runner.jobs`                         | integer          | `1`                                       | Concurrent consumer processes (1-64).                                                                                                                             |
| `runner.fail_fast`                    | boolean          | `false`                                   | Stop starting new processes after the first failure.                                                                                                              |
| `runner.inherit_env`                  | boolean          | `true`                                    | Start from Schepherd's environment before applying `runner.env`.                                                                                                  |
| `runner.max_args_bytes`               | integer          | platform dependent                        | Budget for argv plus the consumer environment (on POSIX); larger batches are split into chunks, a single file that cannot fit is a configuration error.           |
| `runner.env`                          | table of strings | `{}`                                      | Extra variables for the consumer.                                                                                                                                 |
| `[[mappings]]`                        | array of tables  | `[]`                                      | Local `file_match` overrides, each with `schema = "<id>"` (a catalog or `[schemas]` ID).                                                                          |
| `schemas."<id>".path`                 | path             | required once the extends chain is merged | JSON Schema file of the project, relative to the file that sets it; only `${NAME}` is expanded. Used in place, never copied. See [Local schemas](#local-schemas). |
| `schemas."<id>".file_match`           | array of strings | none                                      | Paths the local schema applies to automatically. They take precedence over catalog `fileMatch`; `[[mappings]]` take precedence over them.                         |

Unknown keys are errors in every file.

## Precedence

```text
explicit CLI flags > SCHEPHERD_* environment variables > merged configuration > defaults
```

A flag counts only when it was given on the command line, so
`--offline=false` overrides `offline = true` in a file, while an absent flag
does not. The environment overrides are `SCHEPHERD_REPOSITORY`,
`SCHEPHERD_CATALOG`, `SCHEPHERD_CACHE_DIR`, `SCHEPHERD_OFFLINE`,
`SCHEPHERD_WORKSPACE` and `SCHEPHERD_TIMEOUT`. `SCHEPHERD_CONFIG` selects the
file when `--config` is absent.

How environment values are read:

- An empty or whitespace-only `SCHEPHERD_*` value counts as unset. Any other
  value is used verbatim, without trimming.
- `SCHEPHERD_OFFLINE` accepts `1`, `t`, `T`, `TRUE`, `true`, `True`, `0`, `f`,
  `F`, `FALSE`, `false` and `False`.
- `SCHEPHERD_TIMEOUT` is a positive Go duration such as `90s` or `5m`; a bare
  number is invalid.
- An invalid `SCHEPHERD_OFFLINE` or `SCHEPHERD_TIMEOUT` is a usage error
  (exit code 2), even when the matching flag is also given.
- `SCHEPHERD_REPOSITORY` and `SCHEPHERD_CATALOG` are checked like the file
  values they replace, and only when they take effect: when `--repository` or
  `--catalog` is given, the matching variable is ignored, even if it is
  invalid.
- Relative `--workspace` and `--cache-dir` values and their variables resolve
  against the current directory.
- A blank `SCHEPHERD_CONFIG` means no configuration file.

`pin` and `mirror` name their repositories as arguments and neither expand
nor validate the `[catalog]` settings, so `digest = "${SCHEPHERD_CATALOG}"`
does not stop `schepherd pin` from running before the digest exists (see
[`pin`](cli.md#catalog-and-discovery)).

## `extends`

- Only local files. There is no remote `extends`.
- Relative entries resolve against the directory of the file that declares
  them, never against the current directory.
- Bases apply left to right, then the declaring file on top.
- Scalars override. Tables (`[runner.env]`, `[registries."<key>"]`, `[limits]`,
  `[catalog]`, `[runner]`, `[schemas."<id>"]`) merge key by key.
- A later file can set only `path` or only `file_match` of a `[schemas]` ID
  declared in a base; IDs it does not mention are kept. Some file of the
  chain must set `path`, otherwise loading fails with "required key is
  missing" at the last declaration of the ID.
- Arrays replace: `runner.args`, `[[mappings]]` and a `file_match` from a
  later file replace the earlier value entirely. Nothing is concatenated.
- `extends` is followed in every file, depth first: a base's own bases apply
  before it, so the set of applied files accumulates.
- Relative path values in any file (the top-level one included) resolve
  against that file's directory; `--workspace`, `--cache-dir` and their
  environment variables resolve against the current directory.
- Path values keep their origin: a relative `workspace`, `cache_dir`,
  `runner.cwd`, `ca_file`, `credentials_file` or `schemas."<id>".path` from a
  base file stays relative to the base file after merging.
- Cycles, missing files, depth above 16 and unknown keys are errors.

## Local schemas

JSON Schemas that live in your own repository can be used without any
registry. Declare each one under a schema ID:

```toml
[schemas."company-config"]
path = "schemas/company-config.schema.json"
file_match = ["config/company.json"]
```

- `path` is required. A relative path resolves against the directory of the
  configuration file that sets it, never the current directory. That can be
  a base reached through `extends`: a project can extend a shared base that
  declares the schema and set only `file_match`. `${NAME}` is expanded. URLs
  are rejected; write `./name:x.json` for a file name that contains a colon.
- `file_match` is optional and uses the [matching dialect](matching.md); when
  given, it must list at least one pattern. Without it the schema is used
  only by ID.
- IDs follow the catalog grammar: lowercase letters, digits, `.`, `_` and
  `-`; the first and last character a letter or digit; at most 128
  characters; no `..`.
- A local ID works wherever a catalog ID does: `path`, `cat`, `export`,
  `resolve`, `list`, `patterns`, `run --schema`, `[[mappings]]` and every
  runner placeholder.
- The file is used in place and never copied into the cache. `path` and
  `{schema}` give its absolute path (a symlink is not resolved), so relative
  `$ref`s to sibling files keep working and edits take effect at once.
  `{schema-ref}` is `local:schemas/company-config.schema.json`.
- `config check` checks every local schema, and `path`, `cat`, `export`,
  `resolve` and `run` check each local schema they use. The file must exist,
  be a regular file (a symlink is followed), be readable, be at most
  `limits.max_schema_bytes`, and hold valid JSON: UTF-8, no duplicate object
  keys, nesting up to 512 levels. Whether it is a valid JSON Schema is left
  to your validator. A problem is a configuration error (exit code 2) naming
  the declaring file and position, the key `schemas.<id>.path` and the
  schema file.
- A local schema with the ID of a catalog entry replaces that entry
  completely. Commands use the local file, and the catalog's `fileMatch` for
  that ID no longer applies; copy the patterns into `file_match` to keep
  them. `list` and `patterns` mark such an entry with `"shadows": true`.
- Commands that use only local schemas need no catalog, registry or cache.
  That covers `path`, `cat` and `export` of a local ID, and `resolve` and
  `run` when every file resolves to a local schema through `--schema`,
  `[[mappings]]` or `file_match`. They work without `[catalog]` and with
  `--offline`, and never create the cache directory. `{cache}` still expands
  to the configured or default path. When no cache directory can be
  determined (no `--cache-dir`, `SCHEPHERD_CACHE_DIR` or `cache_dir`, and no
  home directory), such a run still works unless the runner uses `{cache}`.
  With `[schemas]` present and a catalog configured, the catalog is loaded
  only when a catalog schema or a catalog `fileMatch` is needed.
- Without `[catalog]`: `list` and `patterns` show only the local schemas. A
  file that no local rule matches is unmatched (exit code 3;
  `--ignore-unmatched` skips it). A catalog ID fails with exit code 2 and
  the message `not declared in [schemas] and no catalog is configured`, and
  so does `catalog`.
- `pin` and `mirror` ignore `[schemas]`, as they ignore `[catalog]`.

`config check --json` prints the effective configuration, including
`schemas`: an object keyed by ID with `path` (absolute), `declaredIn` (the
file that set `path`) and `fileMatch`. [`examples/local.schepherd.toml`](https://github.com/ovineko/schepherd/blob/main/examples/local.schepherd.toml)
declares a local schema next to the pinned catalog.

## Interpolation

Two separate namespaces, expanded in **one pass**. A substituted value is
never scanned again, so an environment value containing `{schema}` stays
literal.

| Syntax         | Meaning                                                                                                                                                                                                                       |
| -------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `${NAME}`      | Variable from Schepherd's own environment. Unset is an error.                                                                                                                                                                 |
| `{schema}`     | Absolute path of the materialized `schema.json`, or of the local schema file itself.                                                                                                                                          |
| `{schema-id}`  | Schema ID.                                                                                                                                                                                                                    |
| `{schema-ref}` | `repository@sha256:<manifest digest>` of a catalog schema; for a local schema, `local:` followed by its path relative to the workspace with `/` separators, or by its absolute path when the file lies outside the workspace. |
| `{file}`       | Absolute path of one input (`per-file`, `stdin`).                                                                                                                                                                             |
| `{files...}`   | All inputs of a batch, each as its own argument (`batch` only).                                                                                                                                                               |
| `{workspace}`  | Absolute workspace root.                                                                                                                                                                                                      |
| `{cache}`      | Absolute cache root. A run on local schemas alone never creates it.                                                                                                                                                           |
| `$${NAME}`     | Literal `${NAME}`.                                                                                                                                                                                                            |
| `{{` / `}}`    | Literal `{` / `}`.                                                                                                                                                                                                            |

Where each form is allowed:

| Field                                                                        | `${NAME}` | runtime placeholders                      |
| ---------------------------------------------------------------------------- | --------- | ----------------------------------------- |
| `catalog.*`, `workspace`, `cache_dir`, registry paths, `schemas."<id>".path` | yes       | none                                      |
| `runner.command`                                                             | yes       | `{workspace}`, `{cache}`                  |
| `runner.cwd`                                                                 | yes       | `{workspace}`, `{cache}`                  |
| `runner.args`                                                                | yes       | all; `{files...}` only as a whole element |
| `runner.env` values                                                          | yes       | all except `{files...}`                   |

A lone `{` or `}`, an unknown placeholder, `{file}` in `batch` mode and
`{files...}` anywhere else are configuration errors. `batch` requires exactly
one `{files...}` element. `per-file` requires `{file}` in `args` or `env`.
`file_match` patterns of `[schemas]` and `[[mappings]]` are taken literally.
Nothing is evaluated by a shell: no `~`, `$()`, backticks, globbing or word
splitting. `runner.env` values are expanded from Schepherd's environment and
the placeholders only; they cannot reference each other.

`runner.command` and `runner.cwd` must not expand to an empty string. A
`${NAME}` that is set but empty there is rejected by `config check` and by
`run` before any consumer starts (exit code 2), exactly like a literal empty
value. Values that contain `{workspace}` or `{cache}` are never empty.

## Executable resolution

`runner.command` is interpolated first. Then:

- An absolute path is used as is.
- A path containing a separator (`./tools/validate`) resolves against
  `runner.cwd`.
- A bare name is looked up in the `PATH` of the **consumer's** effective
  environment (after `inherit_env` and `runner.env`), skipping empty and
  relative `PATH` entries. The current directory is never searched
  implicitly.

The recommended pattern is an explicit absolute path from the environment,
for example `command = "${CHECK_JSONSCHEMA_BIN}"`. A command that cannot be
found or executed fails with exit code 7 before any consumer starts.

On Windows, additionally:

- `PATHEXT` is read from the consumer's effective environment; when it is
  unset or empty, `.COM;.EXE;.BAT;.CMD` applies. A command that already has
  an extension is used as named when that file exists; otherwise each
  `PATHEXT` extension is tried in order. In `PATH` lookups the directories are
  searched in order and the first match wins. A file without one of these
  extensions is not started.
- A command that resolves to a `.bat` or `.cmd` file is refused with exit
  code 7, even when a real executable exists later in `PATH`. Windows runs
  such files through `cmd.exe`, which parses the whole command line again, so
  file names would be interpreted by a shell. This includes the `.cmd` shims
  that npm creates for package binaries. Point `runner.command` at a real
  executable instead: for a validator installed with npm, the absolute path of
  `node.exe` with the package's JavaScript entry point as the first element of
  `runner.args`; for one installed with pip, the `.exe` launcher in the
  environment's `Scripts` directory.
- A drive-relative command such as `C:tools\validate.exe` is a configuration
  error (exit code 2); use an absolute path.

## Registries and credentials

Credentials are read from Docker-compatible configuration: the file named by
`credentials_file` for that entry, otherwise the standard Docker config
location (`DOCKER_CONFIG` or `~/.docker/config.json`). Credential helpers
(`credsStore`, `credHelpers`) are supported; the helper binary must be
installed separately. Passwords never appear as command-line arguments.
Inside a credentials file, `auths` are looked up by the registry host, and
credentials are only sent to that host. No Docker daemon is needed.

HTTPS is the default. `plain_http = true` allows HTTP for exactly the
repositories its entry applies to and is meant for local registries.
Schepherd never falls back from HTTPS to HTTP. There is no option to disable
certificate verification; add the private CA with `ca_file` instead.

### Per-repository settings

A `[registries."<key>"]` key is either a host (`"ghcr.io"`,
`"localhost:5000"`) or a repository path prefix (`"ghcr.io/org-a"`,
`"localhost:5000/team/schemas"`). For each repository, Schepherd uses the
entry with the longest key that equals its host or is a path prefix of it,
matched on whole path components: `"ghcr.io/org"` applies to `ghcr.io/org`
and `ghcr.io/org/x`, but not to `ghcr.io/organization`.

- The chosen entry applies on its own. `plain_http`, `ca_file` and
  `credentials_file` are not inherited from a shorter key, so repeat them in
  the prefix entry when you need them there.
- Entries never share credentials or cached tokens, even on the same host.
- Path components must be lowercase letters and digits separated by `.`,
  `_`, `__` or one or more `-`. A scheme, tag, digest, empty component or
  trailing `/` is a configuration error (exit code 2).
- Registry hosts must be lower case too (`ghcr.io`, not `GHCR.io`), because
  the registry client matches keys verbatim. An upper-case host in
  `catalog.repository`, `SCHEPHERD_REPOSITORY`, `--repository` or a
  `[registries]` key is a configuration error (exit code 2) whose message
  gives the lower-case spelling. A port must be between 1 and 65535, and a
  bracketed host must be an IPv6 address (`[fe80::1]:5000`). `config check`
  reports all of these.

Two organizations on one host, each with its own account:

```toml
[registries."ghcr.io/org-a"]
credentials_file = "./org-a.json"

[registries."ghcr.io/org-b"]
credentials_file = "./org-b.json"
```

With `--offline`, no credential file is read and no helper runs.
