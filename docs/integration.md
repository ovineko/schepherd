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
- The validator's absolute path is passed through the environment, for example
  `CHECK_JSONSCHEMA_BIN`, and referenced as `command = "${CHECK_JSONSCHEMA_BIN}"`.
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

The wiring below follows the `Tool` and `App` types that datamitsu publishes
for its configuration file. It is a template: fill in the values in angle
brackets.

```js
// datamitsu.config.js (excerpt)
export default (input) => ({
  ...input,
  apps: {
    ...input.apps,
    schepherd: {
      description: "Pinned JSON Schemas from OCI registries",
      binary: {
        version: "<X.Y.Z>", // the client release, for example 0.1.0
        binaries: {
          // one entry per OS/arch with the release asset URL and its SHA-256
        },
      },
    },
  },
  tools: {
    ...input.tools,
    "json-schema": {
      name: "JSON Schema validation through Schepherd",
      operations: {
        lint: {
          app: "schepherd",
          scope: "repository",
          granularity: "file",
          args: ["--config", "{root}/schepherd.toml", "run", "--ignore-unmatched", "--", "{files}"],
          // positive patterns of `schepherd patterns --json` and of the local
          // [[mappings]], translated as described above; negatives are left out
          globs: ["**/package.json", "**/.github/workflows/*.yml"],
          env: {
            CHECK_JSONSCHEMA_BIN: "<absolute path of the validator>",
          },
        },
      },
    },
  },
});
```

Notes:

- Datamitsu expands `{files}` into one argument per file, which matches
  `run -- <files...>`.
- Datamitsu `globs` do not support `!` negation, and `excludeGlobs` applies
  to the whole operation. That is why the template passes
  `--ignore-unmatched` and leaves negative patterns out, as described in
  [Translating patterns](#translating-patterns-into-plain-globs). For exact
  per-schema exclusions, define one operation per schema with
  `run --schema <id>` and that schema's negatives in its `excludeGlobs`.
- Set `workspace = "."` in `schepherd.toml` at the repository root, so
  Schepherd matches paths relative to the same root as the globs.
- Keep `schepherd.toml` explicit (`--config`): Schepherd never discovers it.
