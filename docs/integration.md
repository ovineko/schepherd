# Integrating with other tools

Schepherd is a native binary with a small, stable command-line contract. Any
orchestrator that can run a process can use it; nothing in Schepherd depends
on a particular orchestrator or validator.

## The contract

| Need                               | Command                                      | Output                                                                                                |
| ---------------------------------- | -------------------------------------------- | ----------------------------------------------------------------------------------------------------- |
| Pin a snapshot once                | `schepherd pin <repo>:catalog-latest --json` | `{repository, tag, digest, revision, schemas}`                                                        |
| Which files have schemas           | `schepherd patterns --json`                  | `[{id, fileMatch, artifact, origin}]` (local schemas: `{id, fileMatch, origin, schemaPath, shadows}`) |
| Which schema a path uses           | `schepherd resolve --file <path> --json`     | `{file, path, schema, origin, artifact}` (local schemas: `schemaPath` instead of `artifact`)          |
| A schema as a file                 | `schepherd path <id>`                        | absolute path + newline                                                                               |
| Validate files with your validator | `schepherd run -- <files...>`                | the consumer's own output and exit status                                                             |

Inputs:

- `SCHEPHERD_REPOSITORY` and `SCHEPHERD_CATALOG` (or `catalog.repository` and
  `catalog.digest` in the configuration) pin the snapshot.
- `SCHEPHERD_CONFIG` or `--config` points at the configuration file that holds
  the `[runner]` section.
- The validator's executable is passed through the environment, for example
  `CHECK_JSONSCHEMA_BIN`, and referenced as
  `command = "${CHECK_JSONSCHEMA_BIN}"`. An orchestrator that manages
  tools, like datamitsu below, resolves the binding itself — never hardcode
  an absolute path of one particular machine.
- Schemas that live in the repository are declared in `[schemas]` with a path
  relative to the configuration and need no registry
  ([Local schemas](configuration.md#local-schemas)).

Recommended flow for an orchestrator:

1. Install the native `schepherd` binary and, separately, the validator.
2. Keep the repository and catalog digest in version control.
3. Generate the file patterns once from `schepherd patterns --json` (or
   read them at run time) and use them for your own file discovery; see
   [Translating patterns](#translating-patterns-into-plain-globs).
4. Pass the discovered files to `schepherd run -- <files...>` (or to
   `run --schema <id> -- <files...>` when you grouped them yourself).
5. Treat the exit status as the validator's; see the
   [exit codes](cli.md#exit-codes) for Schepherd's own failures.

Schepherd does not walk directories. Whoever calls it decides which files
exist.

## Tested validator configurations

These configurations live in
[`examples/`](https://github.com/ovineko/schepherd/tree/main/examples), and
the end-to-end suite runs them unchanged against the versions below.
Swapping one validator for another changes only configuration.
`examples/local.schepherd.toml` adds a schema of the repository itself to the
check-jsonschema configuration.

| Validator                           | Version tested | Configuration                              | Invalid document exit status |
| ----------------------------------- | -------------- | ------------------------------------------ | ---------------------------- |
| check-jsonschema                    | 0.38.2         | `examples/check-jsonschema.schepherd.toml` | 1                            |
| Sourcemeta `jsonschema` CLI         | 16.12.0        | `examples/schepherd.toml`                  | 2                            |
| Sourcemeta `jsonschema` CLI (stdin) | 16.12.0        | `examples/stdin.schepherd.toml`            | 2                            |

check-jsonschema reads JSON, YAML and TOML documents by file extension; the
Sourcemeta CLI reads JSON and YAML. Schepherd converts nothing: the format
support is the validator's.

## Translating patterns into plain globs

An orchestrator that discovers files itself usually takes a flat list of
globs. Schepherd's [matching dialect](matching.md) maps onto such globs
(doublestar syntax, relative to the workspace root) with three rules:

| Schepherd pattern                                | Glob                                  | Rule                                                |
| ------------------------------------------------ | ------------------------------------- | --------------------------------------------------- |
| `package.json`, `*.schema.json`                  | `**/package.json`, `**/*.schema.json` | no `/`: a basename at any depth, so prefix `**/`    |
| `/tsconfig.json`, `./renovate.json`              | `tsconfig.json`, `renovate.json`      | a leading `/` or `./` anchors at the root: strip it |
| `**/.github/workflows/*.yml`, `config/**/*.toml` | unchanged                             | contains `/`: already a whole relative path         |
| `!**/node_modules/**`                            | none                                  | negative: see below                                 |

`*`, `?`, `[abc]`, `[!abc]`, `{a,b}`, `**` and `\` escapes mean the same in
both. Both are case-sensitive, and dot-directories such as `.github` must not
be skipped by the orchestrator's own file discovery. `schepherd patterns
--json` lists the catalog patterns and the `file_match` of local `[schemas]`;
add the `file_match` patterns of your `[[mappings]]` yourself. The list
depends on the pinned catalog and the configuration, so generate it again
whenever `SCHEPHERD_CATALOG` or `[schemas]` changes, for example with:

```bash
schepherd --config schepherd.toml patterns --json | jq -r '
  [.[].fileMatch[] | select(startswith("!") | not)
   | if test("^\\.?/") then sub("^\\.?/"; "") elif contains("/") then . else "**/" + . end]
  | unique[]'
```

A negative pattern excludes files from **its own rule only**: a file that one
schema excludes may still match another schema. A flat glob list cannot say
that, and the two ways to handle it behave differently:

- **One operation for all schemas (recommended).** Give the orchestrator the
  positive globs only and pass `--ignore-unmatched` to `run`. A file that the
  globs select but a negative pattern excludes from every schema is then
  skipped by Schepherd, listed on standard error and in the `--report` file as
  `unmatched`, instead of failing the run with exit code 3. Ambiguous files
  still fail. Do not move negative patterns into a tool-wide exclude list:
  that would hide the file from every schema, not only from the one that
  excludes it.
- **One operation per schema.** Use `run --schema <id> -- <files...>` with
  that schema's positive globs and its negative patterns (with the same
  translation, `!` removed) as that operation's own exclusions. This is exact
  per schema, but `--schema` bypasses local `[[mappings]]` and the ambiguity
  check, and it needs one operation per schema ID.

## Datamitsu

Datamitsu discovers files by globs and installs the pinned tools; Schepherd
gives each file a schema and runs the validator over it. There are two ways
to connect them.

### Through datamitsu-config

The [`@shibanet0/datamitsu-config`](https://www.npmjs.com/package/@shibanet0/datamitsu-config)
package wires Schepherd in for you: its `schepherd` tool runs as part of
every `pnpm dm check` over the JSON and YAML files of your repository.

- **One catalog per release.** A release of the package pins the Schepherd
  client, one catalog digest and the validator together. A new catalog
  arrives only with a new release of the package, never in the middle of a
  run, and your project never sees or commits `schepherd.toml` for this.
- **What is skipped.** Files no schema matches, files whose schema no
  configured validator supports, formats nobody reads, and files several
  catalog schemas match (a bare `manifest.json`, for example) are skipped
  rather than failing the check; the report's `skipped` array names each
  file with its reason. A file that several of your own `[schemas]` or
  `[[mappings]]` match is still an error, not a skip.
- **Your own schemas.** Eject the tool's managed configuration into the
  repository and declare schemas there; the package keeps rewriting
  `config_version` and `catalog.digest`, everything else is yours:

```js
// datamitsu.config.js
function getConfig(config) {
  return { ...config, ejectConfigs: ["schepherd"] };
}
globalThis.getConfig = getConfig;
```

```bash
pnpm dm config reconcile --tools schepherd
```

```toml
# schepherd.toml (now owned by your repository)
[schemas."company-config"]
path = "schemas/company-config.schema.json"
file_match = ["config/company.json"]
```

Files named `.json`, `.yaml` and `.yml` reach Schepherd already; for other
names add a glob under `tools.schepherd.operations.lint.globs` in your own
`getConfig`. Turn the tool off with `skip: true`.

### In your own datamitsu configuration

The wiring follows the `Tool` and `App` types datamitsu publishes for its
configuration file. The configuration entry points are the global
`getConfig` and `getMinVersion` functions (an `export default` function is
not one). It is a template: fill in the values in angle brackets.

```js
// datamitsu.config.js
const REPOSITORY = "ghcr.io/ovineko/schepherd-schemas";
const CATALOG = "sha256:<catalog digest>"; // schepherd pin <repo>:catalog-latest --json

function getMinVersion() {
  return "<X.Y.Z>"; // the datamitsu release you need
}

function getConfig(config) {
  return {
    ...config,
    apps: {
      ...config.apps,
      jsonschema: {
        binary: {
          version: "<validator version>",
          binaries: {
            // one entry per OS/arch: the release asset URL and its SHA-256
          },
        },
      },
      schepherd: {
        binary: {
          version: "<X.Y.Z>", // the Schepherd client release
          binaries: {
            // one entry per OS/arch: the release asset URL and its SHA-256
          },
        },
        dependsOn: ["jsonschema"],
        runtimeEnv: {
          JSONSCHEMA_BIN: "${APP_BIN:jsonschema}",
          SCHEPHERD_REPOSITORY: REPOSITORY,
          SCHEPHERD_CATALOG: CATALOG,
        },
        versionCheck: { args: ["version"] },
      },
    },
    tools: {
      ...config.tools,
      schepherd: {
        name: "JSON Schema validation through Schepherd",
        operations: {
          lint: {
            app: "schepherd",
            scope: "repository",
            granularity: "file",
            args: [
              "--config",
              "{root}/schepherd.toml",
              "--workspace",
              "{root}",
              "--quiet",
              "run",
              "--ignore-unmatched",
              "--",
              "{files}",
            ],
            // positive patterns of `schepherd patterns --json` and of the
            // local [[mappings]], translated as described above; negatives
            // are left out
            globs: ["**/package.json", "**/.github/workflows/*.yml"],
          },
        },
      },
    },
  };
}

globalThis.getConfig = getConfig;
globalThis.getMinVersion = getMinVersion;
```

```toml
# schepherd.toml at the repository root; the catalog arrives through the
# environment above, so it is not duplicated here
config_version = 1

[runner]
command = "${JSONSCHEMA_BIN}"
```

Notes:

- The validator is bound with `dependsOn` and referenced as
  `${APP_BIN:jsonschema}` in `runtimeEnv` — never through an absolute path
  of the machine that wrote the configuration. Datamitsu expands the
  binding to the validator's exact executable at run time.
- `SCHEPHERD_REPOSITORY` and `SCHEPHERD_CATALOG` in `runtimeEnv` pin the
  snapshot for every run, overriding anything `schepherd.toml` says; the
  digest lives in one place, the `CATALOG` constant.
- Datamitsu expands `{files}` into one argument per file, which matches
  `run -- <files...>`.
- Datamitsu `globs` do not support `!` negation, and `excludeGlobs` applies
  to the whole operation. That is why the template passes
  `--ignore-unmatched` and leaves negative patterns out, as described in
  [Translating patterns](#translating-patterns-into-plain-globs). For exact
  per-schema exclusions, define one operation per schema with
  `run --schema <id>` and that schema's negatives in its `excludeGlobs`.
- `--workspace {root}` makes Schepherd match paths against the same root
  the globs are relative to; there is no need to set `workspace = "."` in
  `schepherd.toml`.
- Keep `schepherd.toml` explicit (`--config`): Schepherd never discovers it.

#### Offline after one preparation (upcoming)

The datamitsu `prepare` step (one-time, per catalog digest) and the
`schepherd materialize` command are planned integrations. Once the
Schepherd release you pin provides them, the app gains
`prepare: { args: ["--config", "", "--offline=false", "--timeout", "20m",
"--repository", REPOSITORY, "--catalog", CATALOG, "--cache-dir",
"${APP_PREPARED}", "--quiet", "materialize"], network: true }`,
`SCHEPHERD_CACHE_DIR: "${APP_PREPARED}"` joins `runtimeEnv`, and
`env: { SCHEPHERD_OFFLINE: "1" }` joins the operation: one download of the
whole pinned catalog per machine, offline checks afterwards, and an OCI
bundle that carries the materialized catalog for air-gapped hosts. Offline
cache misses then fail with exit code 6 instead of reaching the network.
