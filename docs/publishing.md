# Publishing schema snapshots

Publishing is a maintainer task performed by `schepherd-publisher`, a separate
binary that is not part of the client distribution. Only the publisher
contacts upstream schema sources. For the public catalog the whole flow runs
every week without a human (see [Catalog automation](automation.md)); this
page describes the tool and the files it works with.

```text
upstream catalog at one commit
  → prepare  (keep what did not change; fetch the rest at that commit, resolve $ref closures, bundle,
              verify, license gate; IDs, reuse, holds and exclusions come from catalog/state.json)
  → diff     (compare with catalog/state.json; no registry)
  → publish  (compare with catalog/state.json, pack what changed, push schemas, then the catalog,
              then write the new state)
  → latest   (once the new state is recorded: point catalog-latest at the catalog it records)
```

```bash
go run ./tools/install-jsonschema          # pinned bundler into .tools/bin (checksum verified)

schepherd-publisher prepare --source sources/schemastore.toml --policy sources/licenses.toml \
  --ids sources/ids.json --state catalog/state.json --out ./prepared --json

schepherd-publisher diff --prepared ./prepared --state catalog/state.json --json

schepherd-publisher publish --prepared ./prepared --repository registry.example/org/schemas \
  --state catalog/state.json --state-out new-state.json --json

# after new-state.json is committed as catalog/state.json, tagged and released:
schepherd-publisher latest --repository registry.example/org/schemas --state catalog/state.json --json
```

| Command   | Flags                                                                                                                                                                                                                        |
| --------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `prepare` | `--source` (required), `--out` (required; missing or empty), `--policy` (required for SchemaStore), `--ids`, `--state`, `--snapshot-dir`, `--jsonschema`, `--jobs`, `--refresh <id>` (repeatable), `--refresh-all`, `--json` |
| `diff`    | `--prepared`, `--state` (both required), `--json`                                                                                                                                                                            |
| `publish` | `--prepared`, `--repository` (both required), `--state`, `--state-out`, `--registry-config`, `--update-latest`, `--now YYYYMMDD.HHMM` (tests and replays), `--json`                                                          |
| `latest`  | `--repository`, `--state` (both required; the file must exist), `--check`, `--registry-config`, `--json`                                                                                                                     |

`--quiet` prints only errors. A missing `--state` file means that nothing has
been published yet.

## Sources

A source file describes where schemas come from. Two kinds exist.

### `kind = "upstream"`: SchemaStore

[`sources/schemastore.toml`](https://github.com/ovineko/schepherd/blob/main/sources/schemastore.toml)
imports the complete [SchemaStore](https://www.schemastore.org/) catalog from
one Git commit of its repository. `commit` is the last imported commit; the
weekly update moves it in the same bot commit as `catalog/state.json`, and
only when the published catalog changes. Maintainers do not move it by hand.

```toml
kind = "upstream"
commit = "05b037b7a2b68ead6893c857db2594bcad9951d4"
tarball_base_url = "https://codeload.github.com/SchemaStore/schemastore/tar.gz"
max_tarball_bytes = 67108864

[dependencies]          # per schema, never per run
max_depth = 8
max_per_schema = 64
max_document_bytes = 16777216
max_total_bytes = 268435456
```

The publisher downloads the repository tarball for that commit once
(`--snapshot-dir` keeps it for later runs), checks the archive strictly (no
absolute paths, `..`, links or devices; size and entry limits; the gzip
trailer), and reads the catalog, the schemas, the test instances, `LICENSE`
and `NOTICE` from it. The catalog is SchemaStore's format, so only the
members Schepherd reads are checked (`$schema`, `version` and `schemas`; in
each entry `name`, `description`, `url`, `fileMatch` and `versions`: exact
key case, no nulls, absolute http(s) URLs without credentials), and any other
member, at the top level or in an entry, is ignored; duplicate keys and the
size and depth limits apply to the whole document. A catalog document that
breaks these rules stops the run; a single entry that breaks them fails with
reason `invalid-metadata`.
Every spelling of a SchemaStore URL (`www.schemastore.org`,
`json.schemastore.org`, with or without `.json`, under `/schemas/json/`, and
the repository's raw URLs) maps to the file of that snapshot, so no live or
mutable URL is consulted for them.

### `kind = "local"`: an explicit list

Used for private sets and for the end-to-end fixtures. Relative paths resolve
against the source file:

```toml
kind = "local"
name = "company"
policy = "licenses.toml"

[[entries]]
id = "company-config"                          # optional; derived from url otherwise
name = "company.json"
description = "Company configuration"
url = "https://schemas.example/company.json"   # provenance; not fetched when file is set
file = "schemas/company.json"
file_match = ["config/company.json"]
license = "MIT"                                # SPDX expression; see Licenses
instances = ["tests/company/valid.json"]       # optional JSON instances for the behaviour check

[[documents]]                                  # serve a dependency URI from a local file
uri = "https://schemas.example/common.json"
file = "schemas/common.json"
license = "MIT"

# Optional: ids = "ids.json" (ID overrides), [dependencies] with the same keys
# as the upstream kind, and [fetch] to relax the fetcher for private or test
# setups: allow_http = true, allow_private_hosts = ["host:port"].
```

Every entry of a local source that was never published is required: if one
is not prepared, `prepare` exits with code 5. A published one is held like
any other published schema (see [Published schemas](#published-schemas)).

## Preparation

For every upstream record the publisher decides one of:

| Status           | Meaning                                                       |
| ---------------- | ------------------------------------------------------------- |
| `included`       | Prepared, verified and part of the snapshot.                  |
| `excluded`       | An explicit, reviewed rule in the license policy excludes it. |
| `pending-review` | No license rule allows the source or one of its dependencies. |
| `failed`         | A technical failure with a stable reason code (see below).    |

Records that share one schema URL are merged into one catalog entry whose
`fileMatch` is the union of theirs. SchemaStore `versions` maps (older schema
variants) are not published; `report.json` keeps them per record.

Output of `prepare --out <dir>`:

| File                   | Content                                                                                                                                                                                                                                                                                                                                                                                 |
| ---------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `prepared.json`        | The upstream commit and tarball digest, the recipe (with the bundler version), entries sorted by ID with metadata, content digests, provenance (source and dependency digests) and license decision (allowing rules, pinned detections and redirects), plus the held and excluded published schemas ([schema](https://github.com/ovineko/schepherd/blob/main/api/prepared.schema.json)) |
| `schemas/<id>.json`    | Prepared schema bytes (none for a reused entry)                                                                                                                                                                                                                                                                                                                                         |
| `notices/<sha256>.txt` | License notices (none for a reused entry)                                                                                                                                                                                                                                                                                                                                               |
| `report.json`          | Every record with its status, reason, license, license detections, verification and, for SchemaStore, its file in the snapshot (`snapshotPath`), the held and excluded published schemas, and `totals`; the only file with a timestamp (`generatedAt`)                                                                                                                                  |

`report.json` accounts for every upstream record. Its `held` (`[{id,
reason}]`) and `excluded` (`[{id, rule}]`) repeat those of `prepared.json`,
and `totals` counts the records by status, the prepared entries (and how many
are `reused`), the `held` and `dropped` published schemas, the dropped
patterns, unpublished version URLs, the verification methods and the
license detection results. `prepare --json` prints `{outDir, source, totals,
regressions, collisions, held, excluded}`; the text summary prints the counts
and one `held: <id> (<reason>)` or `excluded: <id> (rule <rule>)` line per
published schema.

Two runs over the same inputs (the state and the answers of license
detection included) produce byte-identical files except the timestamp in
`report.json`.

### Self-contained schemas

A consumer receives one file, so each artifact must be self-contained:

- A schema without external references is only compacted (insignificant
  whitespace removed); its data, number literals and member order included,
  is unchanged.
- A schema with external references is bundled. The publisher discovers the
  reference closure with the Sourcemeta `jsonschema` CLI (`inspect`),
  fetches each dependency once (SchemaStore URLs from the snapshot, others
  over HTTPS, only when the license policy allows the dependency and the
  final URL of any redirect), and embeds every dependency under its own `$id`
  with `jsonschema bundle` (`definitions` for draft-04/06/07, `$defs` for
  2019-09 and later). References keep their original URIs and resolve to the
  embedded resources. A document without `$id` first gets one (`id` for
  draft-04) equal to the URI it was retrieved from.
- The bundler re-serializes bundled schemas: members are reordered and
  numbers normalized (`2.50` becomes `2.5`, `1e2` becomes `1e+2`, `-0`
  becomes `0`). Values, annotations and unknown keywords are preserved.
- **Fragment roots.** When an upstream URL points into a document
  (`…/doc.json#/definitions/x` or `…/doc.json#anchor`), the entry is that
  subschema: the root is
  `{"$schema": <document dialect>, "allOf": [{"$ref": "<document $id>#<fragment>"}]}`
  and the whole document is embedded as a resource, so every reference
  inside it resolves as in the original. The derived ID appends the
  fragment's last segment (`ansible.json#/$defs/tasks` → `ansible-tasks`).
  Upstream tests are instances of the whole document, so fragment entries are
  `structural-only`. License rules match the document URL; rule `urls` must
  not contain a fragment.
- **Top-level `$ref` in draft-04/06/07.** Those dialects ignore every sibling
  of a top-level `$ref`, so a document (root or dependency) that must be
  bundled is rewritten first: `"$ref": X` becomes `"allOf": [{"$ref": X}]` in
  place. This keeps the meaning only when every sibling is inert (`$schema`,
  `$id`/`id`, `$comment`, `definitions`, annotations, unknown keywords); a
  sibling that constrains instances (`type`, `properties`, `required`, …)
  fails with `top-level-ref-draft7`, because validators ignore it while
  editors apply it. When a rewritten root declares an `$id` other than its
  retrieval URL (SchemaStore files declare their `json.schemastore.org`
  alias), that identifier becomes effective: the publisher fetches it (only
  when the license policy allows it) and requires byte-identical content
  (`id-mismatch` otherwise).
- The official JSON Schema meta-schemas are never bundled.

Every bundle is verified before it is accepted:

1. it carries exactly the `$ref`, `$dynamicRef`, `$recursiveRef`, `$anchor`,
   `$dynamicAnchor` and `$recursiveAnchor` members of the documents it was
   made from;
2. `jsonschema inspect` reports no external references;
3. it compiles offline with an independent implementation
   (`santhosh-tekuri/jsonschema`) and a loader that refuses every URL;
4. for every upstream JSON test instance (positive and negative), the original
   schema and the bundle give the same verdict.

`report.json` records the verification of each prepared entry as
`{method, valid, invalid, skippedNonJSON}`:

| Method               | Meaning                                                                                                                                                        |
| -------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `compact-only`       | Not bundled; the upstream bytes without insignificant whitespace.                                                                                              |
| `behaviour-compared` | The bundle agreed with the original on at least one JSON test instance.                                                                                        |
| `structural-only`    | Checks 1–3 only, because no JSON test instance exists (SchemaStore tests are often YAML or TOML, counted in `skippedNonJSON`, and fragment entries have none). |

`prepare` warns about bundled entries that only structural checks verified
and names them.

### Failure reasons

| Reason                 | Meaning                                                                                                                                                            |
| ---------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `fragment-root`        | The upstream URL's fragment is neither a JSON Pointer nor a plain-name anchor, or the document is not a JSON object.                                               |
| `undeclared-dialect`   | The schema has external references, or the URL names a fragment of it, but it has no `$schema`.                                                                    |
| `unsupported-dialect`  | `$schema` is not draft-04/06/07, 2019-09 or 2020-12.                                                                                                               |
| `top-level-ref-draft7` | A draft-04/06/07 document that must be bundled has a top-level `$ref` next to keywords that constrain instances (named in the detail).                             |
| `id-mismatch`          | A dependency declares a different `$id` than the URL it was fetched from, or a rewritten draft-04/06/07 root declares an `$id` that does not serve the same bytes. |
| `unresolved-ref`       | A reference cannot be resolved.                                                                                                                                    |
| `dependency-limit`     | Depth, count or size limits of the closure were exceeded.                                                                                                          |
| `fetch-failed`         | A source or dependency could not be fetched.                                                                                                                       |
| `invalid-json`         | Not well-formed JSON.                                                                                                                                              |
| `duplicate-keys`       | An object has duplicate member names.                                                                                                                              |
| `invalid-schema`       | The verifying implementation rejects the schema.                                                                                                                   |
| `invalid-instance`     | An upstream test instance is not strict JSON or too large.                                                                                                         |
| `invalid-metadata`     | Name, description, dialect, patterns, provenance or license decision violate the catalog or state format, or the notice is not UTF-8 text.                         |
| `too-large`            | The prepared schema or its notice is larger than clients accept by default (64 MiB and 1 MiB).                                                                     |
| `bundler-error`        | The bundler failed.                                                                                                                                                |
| `not-self-contained`   | The bundle still references external resources.                                                                                                                    |
| `behaviour-mismatch`   | Bundle and original disagree on a test instance.                                                                                                                   |
| `reference-mismatch`   | The bundle does not carry exactly the reference and anchor members of its source documents.                                                                        |

Unsupported `fileMatch` patterns (extended globs such as `!(config).yml`) are
dropped from an otherwise included entry and reported as
`pattern-unsupported`. A missing schema is never replaced by a permissive `{}`
or `true`, and upstream schemas are never "fixed".

### Published schemas

`prepare --state catalog/state.json` reads the state of the last publication.
The state keeps IDs stable, lets unchanged schemas keep their artifact and
names every published schema; the prepared set accounts for each of them as
an entry, as held or as excluded.

**A published schema is never dropped automatically and never fails the
run.** When the run cannot refresh it, it is **held**: the next catalog keeps
its last published entry and artifact, `prepare` logs a warning, and
`prepared.json`, the result and `report.json` list it under `held` with one
of these reasons:

| Reason                     | Why the published schema was not refreshed                                                                                                                                          |
| -------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `removed-upstream`         | No upstream record maps to its ID any more: upstream removed the record, or an ID override moved its source to another ID.                                                          |
| `fetch-failed`             | Its source or a dependency could not be downloaded.                                                                                                                                 |
| `license-detection-failed` | Automatic license detection could not answer: a failed request or rate limit (`fetch-failed`), or a URL it cannot pin (`unsupported-host`).                                         |
| `license-refused`          | Automatic license detection found no usable license, one that asserts nothing, or one the `[auto]` allow list does not permit (`no-license`, `not-asserted`, `not-permissive`).     |
| `license-review`           | The license policy holds it for review: a `review` rule, no rule at all, a refused redirect, notices over 768 KiB, or a license declaration of a local source that asserts nothing. |
| `prepare-failed`           | Any other failure: a malformed upstream catalog entry, or bundling, compiling, verification, the catalog's rules or the client's size limits rejected the new content.              |

A later run that refreshes the schema ends the hold.

**Only an explicit `exclude` rule removes a published schema from the
catalog**, a human decision in `sources/licenses.toml` (for example after a
takedown request). The rule is matched against the source as upstream lists
it now and against the source, dependencies and redirect targets the state
records, so it also removes a schema upstream no longer lists. `prepare`
lists it under `excluded` with the rule's ID, `publish` drops it from the
catalog, and the state keeps its record with `excludedRevision`, so the ID
stays reserved. When the rule goes away, the schema returns as `added` and
keeps its recorded artifact when its content is unchanged.

`prepare` exits 5 (after writing its output) only when an entry of a local
source that was never published was not prepared, even over an exclude rule.
SchemaStore records never cause that: new records that fail stay out of the
catalog and appear in the report.

IDs of the state stay reserved, excluded ones included, and so does an old ID
left behind when an ID override renamed a published schema: it is held as
`removed-upstream`, and no other source can take it. Its stand-in source
`urn:schepherd:reserved-id:<id>` may appear in the collisions of
`report.json`.

### Reusing unchanged schemas

A published schema whose inputs did not change is not prepared again. For an
upstream record whose source the state lists under the same ID, `prepare`
first decides the license again from the state alone: the current policy
rules applied to the source, the recorded dependencies and redirect targets,
and the detection findings the state recorded (`licenseDecision`). No
detection service is contacted.

- An `exclude` rule that now matches excludes the schema; a `review` rule
  holds it as `license-review`; a recorded detected license the `[auto]` allow
  list no longer permits holds it as `license-refused`.
- When the policy still allows it on the same rules, findings and license
  expression, `prepare` fetches the source and every recorded dependency.
  When each has its recorded digest and is served as recorded (directly, or
  through the redirect recorded in `licenseDecision.redirects`), the entry is
  **reused**: it keeps the recorded artifact, digests, dialect, provenance and
  license decision, and takes its name, description and `fileMatch` fresh
  from upstream. Nothing is bundled or verified. A change of that metadata
  alone is published as `metadataChanged` with the same artifact.
- A source or dependency that cannot be fetched holds the schema as
  `fetch-failed`. Anything else (a changed digest, a new, changed or dropped
  redirect, another decision) prepares the schema in full, which asks
  license detection again.

A week without upstream changes therefore costs the published schemas no
detection request and no bundling. A source that was never published has no
recorded decision and is detected again every run.

`--refresh <id>` (repeatable) and `--refresh-all` prepare published schemas
in full although their inputs did not change: that is how a recipe, bundler,
notice or policy change reaches them. Each `--refresh` ID must be a schema of
the state (exit code 2 otherwise). A refreshed schema whose bytes and notice
come out the same keeps its artifact.

### The prepared set

`prepared.json` lists the entries of the next catalog, sorted by ID, and the
published schemas it does not carry: `held` (`[{id, reason}]`) and `excluded`
(`[{id, rule}]`). A record the state already marks excluded appears only when
it returns. A prepared entry names its files (`schema`, and `notice` when it
has one); a reused entry has `reusedArtifact` (the recorded schema manifest
descriptor) and `noticeDigest` instead. `licenseDecision` records the
allowing rules and detections and the redirects that served its documents
(`url`, `target`, `digest`, sorted by `url`).

```json
{
  "formatVersion": 1,
  "recipe": "schepherd-prepare/2+sourcemeta-jsonschema-16.12.0",
  "source": {
    "kind": "schemastore",
    "commit": "05b037b7a2b68ead6893c857db2594bcad9951d4",
    "tarballDigest": "sha256:…"
  },
  "entries": [
    {
      "id": "package",
      "name": "package.json",
      "description": "NPM configuration file",
      "dialect": "http://json-schema.org/draft-07/schema#",
      "fileMatch": ["package.json"],
      "schema": "schemas/package.json",
      "contentDigest": "sha256:…",
      "notice": "notices/<sha256 hex>.txt",
      "provenance": {
        "source": "https://www.schemastore.org/package.json",
        "sourceDigest": "sha256:…",
        "license": "Apache-2.0"
      },
      "licenseDecision": { "rules": ["schemastore"] }
    },
    {
      "id": "tool",
      "name": "Tool",
      "fileMatch": ["tool.json"],
      "contentDigest": "sha256:…",
      "noticeDigest": "sha256:…",
      "reusedArtifact": {
        "mediaType": "application/vnd.oci.image.manifest.v1+json",
        "digest": "sha256:…",
        "size": 512
      },
      "provenance": {
        "source": "https://github.com/owner/repo/raw/main/tool.json",
        "sourceDigest": "sha256:…",
        "license": "MIT"
      },
      "licenseDecision": {
        "detections": [
          {
            "url": "https://raw.githubusercontent.com/owner/repo/refs/heads/main/tool.json",
            "source": "github:owner/repo@<commit>",
            "license": "MIT",
            "licenseFile": "LICENSE",
            "licenseDigest": "sha256:…",
            "verdict": "allow"
          }
        ],
        "redirects": [
          {
            "url": "https://github.com/owner/repo/raw/main/tool.json",
            "target": "https://raw.githubusercontent.com/owner/repo/refs/heads/main/tool.json",
            "digest": "sha256:…"
          }
        ]
      }
    }
  ],
  "held": [{ "id": "delta", "reason": "license-detection-failed" }],
  "excluded": [{ "id": "taken-down", "rule": "takedown" }]
}
```

`publish` and `diff` accept a prepared set only together with the state it
was prepared with (see [Comparing with the state](#comparing-with-the-state)).

### The bundler and the recipe

The bundler is the Sourcemeta `jsonschema` CLI, pinned in
`tools/pins/jsonschema.json` (version and SHA-256 of every platform asset) and
installed with `go run ./tools/install-jsonschema`. It is AGPL-3.0 licensed,
runs only as a separate process on the maintainer side, without network
access, and is never distributed with Schepherd.

`prepared.json` records the recipe
`schepherd-prepare/2+sourcemeta-jsonschema-<pinned version>`, also pinned as
a constant in `api/prepared.schema.json`. `publish` refuses a prepared set
made with another recipe (exit code 2). A bundler upgrade reaches published
schemas only through `--refresh-all`; each whose bytes change is then
published as `changed`. Golden files in `testdata/publisher/bundle/*.golden`
pin the bundler's exact output: after an upgrade, review their diff and
regenerate them with
`go test ./internal/publisher/bundle -run TestPinnedBundlerOutput -update`.

### Fetch safety

Per schema: at most 8 levels and 64 dependency documents, 16 MiB per document
and 256 MiB in total. By default the fetcher accepts HTTPS only, refuses
private, loopback and link-local addresses (checked on the dialed IP),
re-validates redirects, refuses downgrades and limits response sizes. A local
source can relax the first two for private or test setups with `[fetch]`.

## Stable IDs

IDs never come from display names:

1. an explicit override in `sources/ids.json` (`{"<source URL>": "<id>"}`;
   any spelling of a SchemaStore URL works as the key, and so does a fragment
   URL);
2. otherwise the ID the same source has in the state (`catalog/state.json`);
3. otherwise a slug of the URL's file name (`…/github-workflow.json` →
   `github-workflow`).

When two sources would get the same ID, the one that already owned it keeps
it and the others get a deterministic suffix; the report lists every such
collision. Renames are done with an override; the old ID stays in the catalog
with its last artifact, held as `removed-upstream`, and stays reserved.

## Licenses

A publicly reachable URL is not permission to redistribute. The policy file
(`sources/licenses.toml`) holds explicit rules:

```toml
[[rules]]
id = "schemastore"
decision = "allow"            # allow | exclude | review
hosts = ["www.schemastore.org", "json.schemastore.org"]
path_prefix = "/"             # optional
license = "Apache-2.0"
notice_file = "notices/schemastore.txt"
reason = "why this decision was made"
```

- A rule matches by exact `urls`, or by `hosts` (optionally narrowed by
  `path_prefix`). `exclude` and `review` rules may also list `file_names`,
  which match every spelling of a file on the rule's hosts under its prefix
  (with or without `.json`, case-insensitive). Inline `notice` text can
  replace `notice_file`.
- For one URL the most specific rule decides: exact `urls`, then
  `file_names` rules, then host rules by the longest `path_prefix`. Across a
  source and its dependencies, `exclude` wins over `review`, which wins over
  `allow`. A schema is included only if its own source **and every bundled
  dependency** are allowed; anything without a matching rule is
  `pending-review` unless automatic detection allows it.
- On `github.com` and `raw.githubusercontent.com` the owner and repository
  segments match in any letter case, as GitHub resolves them, so a rule for
  `/SchemaStore/schemastore/` also covers `/schemastore/SchemaStore/`.
- A license value that asserts nothing never allows publication. An `allow`
  rule's `license` must be an SPDX license expression that does not use
  `NOASSERTION`, `NONE`, `OTHER`, `UNKNOWN` or `UNLICENSED` (any letter case)
  as an identifier; `LicenseRef-…` is fine, npm's `SEE LICENSE IN <file>` and
  free text such as `Apache 2.0` are not. A policy with such an allow rule is
  refused (exit code 2); `exclude` and `review` rules may record such a value
  for information. A local source entry or `[[documents]]` declaration with
  such a license is held for review even when an allow rule covers its URL,
  and a `provenance.license` in `prepared.json` that fails the check makes
  the prepared set invalid for `publish` (exit code 5).

The shipped policy allows files of the SchemaStore repository under its
repository license (Apache-2.0), holds back four files whose comments mention
third-party content (under every spelling of their URLs), and enables
automatic detection for everything else. The notice layer of a SchemaStore
file carries the rule's notice (`sources/notices/schemastore.txt`) followed
by the repository's `LICENSE` (whose section 4(a) requires copies to carry
it) and `NOTICE` files from the snapshot.

`THIRD_PARTY_NOTICES.md` lists the schemas of the recorded catalog grouped by
the source of their license. It is generated by
`go run ./tools/release notices generate`, and the weekly job regenerates and
commits it with every catalog revision it records.

### Automatic license detection

Sources outside the SchemaStore repository that no rule matches get their
license detected at a pinned point, and a permissive license on the allow
list publishes them:

```toml
[auto]
enabled = true          # default false when the section or the key is absent
allow = ["MIT", "MIT-0", "Apache-2.0", "BSD-2-Clause", "BSD-3-Clause", "ISC", "0BSD", "CC0-1.0", "Unlicense", "BlueOak-1.0.0"]
hosts = ["api.github.com", "registry.npmjs.org", "unpkg.com", "cdn.jsdelivr.net"]
```

- `allow` lists SPDX license identifiers, compared case-insensitively; the
  default is the list above. An empty list, an expression, a value that
  asserts nothing, a `LicenseRef-…` or `DocumentRef-…` identifier (whose
  meaning differs per source) or a duplicate is a usage error (exit code 2).
- `hosts` lists the services detection may contact, a subset of the four
  above (default: all). Unknown keys and keys in the wrong letter case are
  refused.

Explicit rules come first, then the license declarations of a local source,
then detection: only URLs that no rule matches are detected.

| Source URL                                                                                                                    | How the license is found                                                                                                                                                                                                                                                                                                                                                                                                               |
| ----------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `raw.githubusercontent.com/<owner>/<repo>/<ref>/<path>`, `github.com/<owner>/<repo>/raw/<ref>/<path>` or `/blob/<ref>/<path>` | The ref (a branch, tag or commit, or `refs/heads/<name>` or `refs/tags/<name>`; a branch containing `/` is read as its first segment) is resolved to a commit through the GitHub REST API; the license comes from `GET /repos/{owner}/{repo}/license?ref=<commit>`, and for Apache-2.0 the `NOTICE`, `NOTICE.txt` or `NOTICE.md` file at that commit. A renamed branch is resolved through `GET /repos/{owner}/{repo}/branches/{ref}`. |
| `unpkg.com/<package>@<version>/<path>`, `cdn.jsdelivr.net/npm/<package>@<version>/<path>`                                     | The version must be exact (no range, tag or build metadata). The license comes from the npm registry metadata of that version; the `LICENSE` or `LICENCE` file (and for Apache-2.0 the `NOTICE` file) from the same CDN at that version.                                                                                                                                                                                               |

Anything else, including URLs with a query, is `unsupported-host`. Each URL
is requested once and each repository ref or package version is detected
once per run. A detected expression allows the source only when it asserts a
license and the allow list permits it: an `OR` needs one permitted operand,
an `AND` all of them (SPDX precedence), and `WITH` exceptions and
`LicenseRef-…` identifiers are never auto-allowed. Everything else stays
`pending-review` with a reason (a published schema is held instead, see
[Published schemas](#published-schemas)):

| Reason             | Meaning                                                                                                                  |
| ------------------ | ------------------------------------------------------------------------------------------------------------------------ |
| `no-license`       | no license file, or its text cannot travel with the schema (over 64 KiB, not UTF-8, control or bidirectional characters) |
| `not-permissive`   | the detected license is not permitted by `allow`                                                                         |
| `not-asserted`     | the detected value names no license (`NOASSERTION`, …) or is not an SPDX expression                                      |
| `unsupported-host` | the URL is not a pinnable GitHub or npm CDN location, or detection may not contact the service it needs                  |
| `fetch-failed`     | a request failed, or a service's rate limit stopped detection                                                            |

An auto-allowed schema carries the license text, and for Apache-2.0 the
`NOTICE` text, in its notice layer:

```text
License of the GitHub repository <owner>/<repo> at <ref> (SPDX: <expression>), file <path>:

<license text>

NOTICE of the GitHub repository <owner>/<repo> at <ref>, file <name>:

<NOTICE text, only for Apache-2.0 when the repository has one>
```

For npm sources the headers name `the npm package <name> version <version>`.
The header names the ref as the URL does, not the resolved commit, so every
schema of one source shares one notice blob and the text changes only when
the license text does. A schema whose combined notices exceed 768 KiB is
held for review.

**GitHub token and rate limits.** The publisher sends `GITHUB_TOKEN`, when
set, to `api.github.com` only. A value with whitespace or control characters
inside, or a token GitHub rejects (HTTP 401), is a usage error (exit code 2),
so a broken token cannot silently keep new schemas out. A full SchemaStore
import needs roughly 900 GitHub requests, far above the 60 anonymous
requests per hour; only the first import, `--refresh-all` and new or changed
sources need detection. When a service refuses a request for its rate limit
and names when the limit resets (`X-RateLimit-Reset` or `Retry-After`),
detection pauses that service and retries: up to 65 minutes with a GitHub
token, up to 2 minutes without one, at most four times per run. A later
reset, no reset time or a fifth limit stops all requests to that service for
the rest of the run; the sources that still need it stay `pending-review`
(`fetch-failed`).

Detection results are recorded where they cannot change the catalog on their
own: `report.json` lists the findings for every document of every decided
record (`licenseDetections`) and counts them in `totals.licenseDetection`;
`prepared.json` and the state record the decision each entry was published
with (`licenseDecision`), which pins the allowing detections (`source` is
`github:<owner>/<repo>@<commit>` or `npm:<package>@<version>`). The catalog
carries only the combined expression in `provenance.license`.

## Comparing with the state

`schepherd-publisher diff --prepared <dir> --state <file> [--json]` compares a
prepared set with the state exactly as `publish` would, without contacting a
registry, and exits 0 whether or not anything changed. The prepared set must
have been prepared with that state: every schema of the recorded catalog must
be an entry, held or excluded, and a reused entry must carry the recorded
artifact. Otherwise `diff`, like `publish`, exits with code 2 and `the
prepared set was not prepared with this publisher state: …`. A prepared set
compared with the state its own publication wrote shows no changes.

```json
{
  "hasChanges": false,
  "added": [],
  "changed": [],
  "metadataChanged": [],
  "held": [{ "id": "tool", "reason": "fetch-failed" }],
  "excluded": [],
  "unchanged": 646
}
```

| Member            | Meaning                                                                                                                                                 |
| ----------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `hasChanges`      | Whether `publish` would make a new revision: `added`, `changed`, `metadataChanged` or `excluded` is not empty. A hold, or its end, never sets it alone. |
| `added`           | IDs new to the catalog: never published, or returning after an exclusion.                                                                               |
| `changed`         | IDs whose artifact changes: new schema bytes or a new notice.                                                                                           |
| `metadataChanged` | IDs that keep their artifact while their entry changes (name, description, `fileMatch`, dialect or provenance).                                         |
| `held`            | Every schema the new catalog keeps with its last entry and artifact, with its reason. Held entries count in `unchanged`.                                |
| `excluded`        | IDs an explicit exclude rule removes now. Records the state already marks excluded are not listed again.                                                |
| `unchanged`       | Entries of the new catalog that equal the recorded ones.                                                                                                |

Every array is present and sorted by ID, and the new catalog has `added +
changed + metadataChanged + unchanged` entries. Without a state everything is
added.

## Publishing

`publish` needs no previous catalog from the registry: the state decides what
is new.

1. **Compare** the prepared set with the state, as `diff` does. **When the
   catalog would not change, the publication is a `noop`:** no registry
   request, no revision, and the state written to `--state-out` is
   byte-identical to `--state`. A hold alone therefore never makes a
   revision. With `--update-latest` a noop only moves `catalog-latest` to the
   state's catalog, after checking that `catalog-<revision>` names exactly
   that catalog index (exit code 5 otherwise). Without `--state` there is never a
   noop: identical content resumes the newest revision instead.
2. **Pack** new and changed content (recipe `schepherd-pack/1`, see
   [OCI format](oci-format.md#packing-recipe-schepherd-pack1)). Identical
   content produces identical digests, independent of the date, the host and
   the repository name.
3. **Choose the revision.** When the newest `catalog-<revision>` in the
   repository is newer than the state's revision and its catalog has the same
   content, that revision is **resumed**: nothing is uploaded again, only
   missing tags are created, and the same state is written. That is what a run
   interrupted before the state was written leaves behind. Otherwise the
   revision is the UTC minute of the publication. It must be newer than every
   revision in the repository and in the state: the same minute with other
   content, or a clock behind, is refused with exit code 2 (`… publish again
in a later minute`). A newest tag in a newer Schepherd wire format stops
   `publish` with exit code 2 before anything is written.
4. **Push schemas.** Missing blobs and manifests are uploaded, without tags
   of their own, and every entry is verified to resolve in the repository. A recorded artifact missing from the repository is pushed
   again when packing reproduces it byte for byte; otherwise `publish` stops
   with exit code 5 before any catalog exists. Reused and held entries carry
   no schema bytes, so publish a set prepared against a state only into the
   registry that state describes, or into a mirror of its catalog.
5. **Push the catalog.** The revision tag is checked first, then
   `catalog.json`, the catalog metadata manifest and finally the catalog
   index, which references the metadata manifest and every schema artifact
   (see [OCI format](oci-format.md#catalog-index)), are pushed and the index
   is tagged `catalog-<revision>`. A minute that another catalog took in the
   meantime gets the same exit-2 refusal. An index larger than 4 MiB, which
   clients and registries refuse, stops `publish` with exit code 5 before
   anything is pushed. Published tags never move, and these are the only
   tags besides `catalog-latest`: the index keeps the schema artifacts alive.
6. **Write the new state** to `--state-out` (which needs `--state` and may be
   the same file; its path is checked before any work starts). When writing
   fails, the catalog exists but `publish` exits non-zero; the next run
   resumes the revision and writes the state.
7. **Only with `--update-latest`**, and only after all of the above
   succeeded, move `catalog-latest`.

Registry settings (plain HTTP, extra CA bundle, credentials file) come from
`--registry-config`, a TOML file with the same `[registries."<key>"]` tables
as the client configuration (see
[Registries](configuration.md#registries-and-credentials)); relative paths in
it resolve against its directory.

### Artifact reuse

An unchanged schema is published once and keeps its artifact digest. Reused
and held entries keep the recorded artifact, and so does a schema prepared in
full whose `contentDigest` and `noticeDigest` equal the recorded ones, even if
packing would produce other bytes today (a new packing recipe or Go
toolchain, for example); such entries are listed in `keptArtifacts` with a
warning. A new notice for unchanged schema bytes is new content: the schema
is uploaded again and reported as `changed`, so an artifact always carries
the notice of the license its entry names.

### Result

`publish --json` prints:

```json
{
  "status": "published",
  "repository": "registry.example/org/schemas",
  "revision": "20260928.0300",
  "catalogDigest": "sha256:…",
  "added": ["new-tool"],
  "changed": ["package"],
  "metadataChanged": ["renovate"],
  "removedUpstream": ["gone"],
  "held": [
    { "id": "gone", "reason": "removed-upstream" },
    { "id": "tool", "reason": "fetch-failed" }
  ],
  "excluded": [],
  "keptArtifacts": [],
  "tags": { "created": ["catalog-20260928.0300", "…"], "existing": ["…"] },
  "catalogSize": 512,
  "unchanged": 646,
  "uploadedSchemas": 2,
  "reusedSchemas": 647
}
```

| Member                                                                 | Meaning                                                                                                  |
| ---------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------- |
| `status`                                                               | `noop`, `published` or `resumed`                                                                         |
| `revision`                                                             | the new revision; for a noop the state's                                                                 |
| `catalogDigest`                                                        | digest of the catalog index, which clients pin (`catalogSize` is its size)                               |
| `added`, `changed`, `metadataChanged`, `held`, `excluded`, `unchanged` | as in [`diff`](#comparing-with-the-state); a noop lists the current holds                                |
| `removedUpstream`                                                      | the IDs of `held` whose reason is `removed-upstream`                                                     |
| `keptArtifacts`                                                        | IDs prepared in full that would pack to another artifact today and kept their recorded one               |
| `tags`                                                                 | tags created (or moved, for `catalog-latest`) and tags that already pointed right                        |
| `uploadedSchemas`                                                      | distinct schema artifacts this run pushed                                                                |
| `reusedSchemas`                                                        | entries that kept the artifact the state records, such as unchanged, held and metadata-only changed ones |

### Moving catalog-latest

`schepherd-publisher latest --repository <repo> --state <file> [--check]`
points `catalog-latest` at the catalog the state records, without a prepared
set. It first checks that `catalog-<revision>` names exactly the recorded
catalog index and that this catalog holds the recorded revision and entries;
otherwise it exits with code 5 and moves nothing. `--check` only verifies. A
missing state file is a usage error (exit code 2). `--json` prints
`{repository, revision, catalogDigest, latest, catalogSize}`, where `latest`
is `moved`, `current` or `checked`.

## State

`catalog/state.json` is the record in Git of what the registry holds. The
publisher decides from it alone whether anything changed, which artifacts and
license decisions to reuse and which IDs stay reserved. Its JSON Schema is
[`api/state.schema.json`](https://github.com/ovineko/schepherd/blob/main/api/state.schema.json).

```json
{
  "formatVersion": 1,
  "source": {
    "kind": "schemastore",
    "repository": "https://github.com/SchemaStore/schemastore",
    "commit": "05b037b7a2b68ead6893c857db2594bcad9951d4",
    "tarballDigest": "sha256:…"
  },
  "recipe": "schepherd-prepare/2+sourcemeta-jsonschema-16.12.0",
  "catalog": { "revision": "20261012.0300", "digest": "sha256:…", "size": 512 },
  "schemas": [
    {
      "id": "tool",
      "contentDigest": "sha256:…",
      "noticeDigest": "sha256:…",
      "firstRevision": "20260928.0300",
      "artifactRevision": "20260928.0300",
      "lastChangedRevision": "20261005.0300",
      "heldSinceRevision": "20261012.0300",
      "heldReason": "fetch-failed",
      "licenseDecision": { "rules": ["tool-vendor"] },
      "entry": { "id": "tool", "artifact": { "…": "…" }, "…": "…" }
    }
  ]
}
```

| Member                          | Meaning                                                                                                                                                                                                                         |
| ------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `source`                        | the upstream input of the current catalog: `kind` `schemastore` with repository, commit and tarball digest, or `kind` `local` with only a `name`                                                                                |
| `recipe`                        | the prepare recipe of the set the current catalog was published from; reused and held entries may keep artifacts of earlier recipes                                                                                             |
| `catalog`                       | revision, digest and size of the current catalog index                                                                                                                                                                          |
| `schemas[].contentDigest`       | SHA-256 of the prepared schema bytes; while it and `noticeDigest` stay the same, the artifact in `entry` is reused                                                                                                              |
| `schemas[].noticeDigest`        | SHA-256 of the notice text; absent when the artifact has none                                                                                                                                                                   |
| `schemas[].firstRevision`       | the revision that added the schema, again after an exclusion                                                                                                                                                                    |
| `schemas[].artifactRevision`    | present only when the last change was metadata-only: the revision that gave the entry its current artifact                                                                                                                      |
| `schemas[].lastChangedRevision` | the revision that last changed the entry or its artifact; a hold or an exclusion does not move it                                                                                                                               |
| `schemas[].heldSinceRevision`   | present while the schema is held: the first published revision that kept it without refreshing it                                                                                                                               |
| `schemas[].heldReason`          | present exactly when `heldSinceRevision` is: the reason of the latest hold                                                                                                                                                      |
| `schemas[].excludedRevision`    | present while an exclude rule keeps the schema out: the revision that dropped it; `entry` is the entry it last had                                                                                                              |
| `schemas[].licenseDecision`     | the decision the entry was published with: allowing rule IDs (`rules`), allowing detections sorted by URL (`detections`) and redirects that served its documents sorted by `url` (`redirects`); absent when all three are empty |
| `schemas[].entry`               | the catalog entry exactly as in `catalog.json`                                                                                                                                                                                  |

Rules the publisher enforces beyond the JSON Schema:

- Schemas are sorted by ID, excluded ones included, and `id` equals
  `entry.id`. The entries that are not excluded form a valid catalog with
  provenance.
- Revisions are real UTC minutes, ordered as follows (a missing
  `artifactRevision` drops out of the first chain):
  - `firstRevision <= artifactRevision < lastChangedRevision <= catalog.revision`
  - `lastChangedRevision < heldSinceRevision <= catalog.revision`
  - `lastChangedRevision < excludedRevision <= catalog.revision`
- A record is never held and excluded at once.
- Redirects are unique and sorted by `url`, a `target` differs from its
  `url`, and both are `http` or `https` URLs of printable ASCII without
  credentials or fragment.

A `formatVersion` other than 1 is a usage error (exit code 2); any other
defect is an integrity error (exit code 5).

What a publication of revision R writes, which is also how the release notes
classify each schema:

| The schema is          | The state records                                                                        | Release notes    |
| ---------------------- | ---------------------------------------------------------------------------------------- | ---------------- |
| added                  | `firstRevision` and `lastChangedRevision` R; no `artifactRevision` or `excludedRevision` | Added            |
| changed                | `lastChangedRevision` R; no `artifactRevision`                                           | Changed          |
| metadata-only changed  | `artifactRevision` the revision of its current artifact, `lastChangedRevision` R         | Metadata updated |
| held                   | `heldSinceRevision` R unless it was already held, and the current `heldReason`           | Held (unchanged) |
| refreshed after a hold | no `heldSinceRevision` or `heldReason`                                                   | by what changed  |
| excluded               | `excludedRevision` R and no hold; `entry` stays as last published                        | Excluded         |

The encoding is canonical (fixed member order, two-space indentation, a final
newline) and has no timestamps, so the file changes only when the published
content changes. Only `schepherd-publisher publish --state-out` writes it,
and in this repository only the weekly bot commits it; never edit it by hand.

## Coverage of the SchemaStore import

Measured at the imported commit `05b037b` with the shipped configuration (the
rules of `sources/licenses.toml` and automatic license detection with a
read-only GitHub token):

- 1,474 upstream records, 990 catalog entries (991 included records, one
  merged into another entry that shares its URL);
- 451 `pending-review`: 447 whose license detection allowed nothing (267
  `unsupported-host`, 66 `no-license`, 56 `not-asserted`, 46
  `not-permissive`, 12 `fetch-failed`) and 4 held by the policy's review
  rules;
- 0 excluded;
- 32 failed: 7 `id-mismatch`, 5 `unsupported-dialect`, 5
  `top-level-ref-draft7`, 5 `invalid-json`, 3 `undeclared-dialect`, 3
  `fetch-failed`, 2 `unresolved-ref`, 1 `invalid-schema` and 1
  `duplicate-keys`;
- 80 bundled (32 behaviour-compared, 48 structural-only), 910 compacted only,
  2 extended-glob patterns dropped, 1,102 version URLs not published.

The published snapshot is therefore **not** "all of SchemaStore"; the report
lists the fate of every record. The numbers move with upstream: the job
summary of every weekly run that prepares upstream shows the current
coverage. To measure it yourself, run `prepare` without `--state` (see
below) and read `jq .totals <out>/report.json`.

## Running it locally

A maintainer can replay the registry side of a weekly run against a local
registry. The Git side (`catalogbot commit` and `release`) is covered by
`tools/catalogbot` tests against a fake GitHub API; never run it by hand
against the real repository. Set `GITHUB_TOKEN` to a read-only token for
`prepare`, or GitHub's anonymous rate limit leaves sources that need
detection out or held.

```bash
pnpm build                                       # dist/bin/schepherd and dist/bin/schepherd-publisher
go run ./tools/install-jsonschema
mkdir -p dist/update
upstream="$(git ls-remote https://github.com/SchemaStore/schemastore refs/heads/master | cut -f1)"
go run ./tools/catalogbot set-commit --source sources/schemastore.toml --commit "$upstream" \
  --out dist/update/schemastore.toml

dist/bin/schepherd-publisher prepare --source dist/update/schemastore.toml --policy sources/licenses.toml \
  --ids sources/ids.json --state catalog/state.json --snapshot-dir dist/update/snapshot \
  --out dist/update/prepared --json > dist/update/prepare.json
dist/bin/schepherd-publisher diff --prepared dist/update/prepared --state catalog/state.json   # what the weekly job would publish

docker run --detach --name schepherd-local-registry --publish 127.0.0.1:5000:5000 \
  distribution/distribution:3.1.2@sha256:d106962e6fe3fa69c178cec77eeeee5edb626b8df6535b7bd6b95e658a0a5ca9
printf '[registries."localhost:5000"]\nplain_http = true\n' > dist/update/registries.toml
printf 'config_version = 1\n[registries."localhost:5000"]\nplain_http = true\n' > dist/update/client.toml
base=dist/update/none.json                       # does not exist: no publication yet
if [ -f catalog/state.json ]; then               # reused and held entries need the recorded artifacts
  dist/bin/schepherd --config dist/update/client.toml mirror \
    "ghcr.io/ovineko/schepherd-schemas@$(jq -r .catalog.digest catalog/state.json)" localhost:5000/local/schemas
  base=catalog/state.json
fi
pub=(dist/bin/schepherd-publisher publish --prepared dist/update/prepared --repository localhost:5000/local/schemas
  --registry-config dist/update/registries.toml --json)
"${pub[@]}" --state "$base" --state-out dist/update/state.json --update-latest > dist/update/publish.json
"${pub[@]}" --state dist/update/state.json                            # status "noop", no registry request
go run ./tools/catalogbot notes --result dist/update/publish.json --state dist/update/state.json \
  --repository localhost:5000/local/schemas
```

A set prepared against `catalog/state.json` can only be published against
that state, and its reused and held entries point at artifacts that only the
recorded catalog holds, which is why the local registry first receives a
mirror of it (the weekly test job does the same). To measure coverage from
scratch, drop `--state` from `prepare` and publish against the missing
`dist/update/none.json`.
