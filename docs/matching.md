# File matching

`resolve`, `run` (without `--schema`) and integrations use the same path
matching dialect. Matching looks only at paths. File contents, inline
`$schema` keys and modelines are never read.

## Paths

- A path is made absolute (relative to the current directory), then relative
  to the workspace root (`--workspace`, `workspace`, default: current
  directory).
- Separators are normalized to `/` on every platform.
- Paths outside the workspace cannot be matched automatically; pass
  `--schema <id>` for them.
- Matching is **case-sensitive** on every platform.

## Patterns

| Pattern form        | Compared with                 | Example                                          |
| ------------------- | ----------------------------- | ------------------------------------------------ |
| no `/`              | the basename, at any depth    | `package.json`, `*.schema.json`, `.eslintrc`     |
| contains `/`        | the whole relative path       | `**/.github/workflows/*.yml`, `config/**/*.toml` |
| leading `/` or `./` | the whole path, from the root | `/tsconfig.json` matches only the root file      |
| leading `!`         | excludes paths (negative)     | `!**/node_modules/**`                            |

Glob syntax: `*` (any characters except `/`), `?` (one character except `/`),
`[abc]`, `[!abc]`, `{a,b}`, `**` as a whole segment (zero or more
directories) and `\` to escape a special character. A `**` inside a segment
(`**.json`) behaves like `*`. Dotfiles and dot-directories such as `.github`
are matched like any other name.

A rule (one catalog entry, one `[[mappings]]` block or one `[schemas]` entry)
matches a path when at least one positive pattern matches and no negative
pattern does.

Extended globs such as `!(config).yml` are **not** supported. The publisher
drops such upstream patterns and lists them in its report instead of
interpreting them differently.

## Precedence

```text
--schema <id>  >  local [[mappings]]  >  local [schemas] file_match  >  catalog fileMatch
```

A catalog entry whose ID is declared in `[schemas]` takes no part in
matching: the local schema replaces it, patterns included (see
[Local schemas](configuration.md#local-schemas)). When a file matches through
a local `file_match`, or through a `[[mappings]]` rule whose `schema` is a
local ID, the catalog is not loaded at all. A mapping to a catalog ID needs
the catalog like any catalog schema: with `--offline` and the catalog not in
the cache, it fails with exit code 6.

Within one level exactly one schema must match. Several matches are an
ambiguity error (exit code 3) that lists every candidate, in a deterministic
order; among local schemas it reads `<path> matches several schemas via
local rules: a, b`. A local mapping resolves the ambiguity:

```toml
[[mappings]]
file_match = ["manifest.json"]
schema = "webextension"
```

## Integration data

`schepherd patterns --json` exports every schema that has patterns together
with its patterns and its manifest descriptor (for a local schema, its file),
so an orchestrator can do its own file discovery without losing the link
between a glob and a schema (see
[Translating patterns into plain globs](integration.md#translating-patterns-into-plain-globs)
for tools whose globs have no negation):

```json
[
  {
    "id": "github-workflow",
    "fileMatch": ["**/.github/workflows/*.yml", "**/.github/workflows/*.yaml"],
    "artifact": {
      "mediaType": "application/vnd.oci.image.manifest.v1+json",
      "digest": "sha256:…",
      "size": 512
    },
    "origin": "catalog"
  },
  {
    "id": "company-config",
    "fileMatch": ["config/company.json"],
    "origin": "local",
    "schemaPath": "/abs/schemas/company-config.schema.json",
    "shadows": false
  }
]
```

A local schema with `file_match` has no `artifact`; `shadows` is `true` when
it replaces a catalog entry with the same ID. `[[mappings]]` are not listed.
