# Processors

Processors are explicit executable bindings, not installed tools. A configuration may
use the existing `runner.command` contract or declare `[processors]`, but never both.
All commands run without a shell. Managed toolchains supply the exact executable
paths through environment bindings; Schepherd contains no dependency version pins.

```toml
config_version = 1

[runner]
processors = ["native", "python", "javascript"]
unsupported = "error"

[processors.native]
type = "sourcemeta"
command = "${JSONSCHEMA_BIN}"

[processors.python]
type = "check-jsonschema"
command = "${CHECK_JSONSCHEMA_FRONTEND}"

[processors.javascript]
type = "ajv"
command = "${AJV_FRONTEND}"
```

A complete catalog-backed example is
[`examples/processors.schepherd.toml`](https://github.com/ovineko/schepherd/blob/main/examples/processors.schepherd.toml).
The Python and AJV commands are Schepherd's owned frontends, not the bare upstream CLIs.
The frontend sources and dependency manifests live in `tools/check-jsonschema/` and
`tools/ajv/`. Bind their executable entrypoints in the same managed environment that
provides their dependencies; do not rely on a system interpreter or adjacent executables
being discovered automatically.

## Selection

If `runner.processors` is omitted, declared processors are ordered by type:
`sourcemeta`, `check-jsonschema`, `ajv`, `custom`; ties sort by processor name.
An explicit list replaces the order and can restrict the candidates. It must be nonempty,
contain no duplicates and reference only declared processors.

A local schema can set `processor = "python"`; a matching `[[mappings]]` entry can
set its own `processor` and takes precedence. An explicit selection tries only that
processor. Conflicting processor selections on matching mappings are errors, never skips.

Before running any document validator, Schepherd checks input format and prepares
the schema with each candidate's actual executable. Verified compiler artifacts and
capability decisions are reused from the schema cache. An unsupported capability
response or schema compilation rejection permits trying the next candidate, before
any document is validated. Schema
files must first be readable, bounded, well-formed strict JSON; malformed JSON fails
immediately, even under the skip policy. Missing binaries, crashes, malformed probe
responses and timeouts fail the run. Once document validation starts, its result is
final: an invalid document is not retried with another validator.

`runner.unsupported` defaults to `"error"` (usage error, exit 2). `"skip"` records each
unsupported file with its reason in the machine report and prints the skip in raw mode.
It does not hide invalid documents, ambiguous mappings or infrastructure errors.

## Fields

| Processor field | Meaning                                                                                                               |
| --------------- | --------------------------------------------------------------------------------------------------------------------- |
| `type`          | Required after `extends` merging: `sourcemeta`, `check-jsonschema`, `ajv`, `custom`.                                  |
| `command`       | Required after merging. Executable template, resolved like `runner.command`.                                          |
| `cwd`           | Consumer working directory; defaults to `runner.cwd`. Relative to its declaring file.                                 |
| `env`           | Extra consumer environment; merges over `runner.env`. Values are runner templates.                                    |
| `inherit_env`   | Defaults to `runner.inherit_env`.                                                                                     |
| `output`        | `sourcemeta-json`, `check-jsonschema-json`, `ajv-json` or `raw`; defaults to the typed JSON format, `raw` for custom. |
| `args`          | Required for `custom`; arbitrary runner argument templates. Typed presets reject overrides.                           |
| `mode`          | Custom runner mode: `batch`, `per-file`, `stdin`; default `per-file`. Typed presets reject overrides.                 |
| `convert`       | Optional explicitly bound yq executable for the formats listed in `formats`.                                          |
| `formats`       | With `convert`, source formats to convert. For custom without conversion, an optional input allowlist.                |

`runner.timeout`, `jobs`, `fail_fast` and `max_args_bytes` remain common execution
settings. `runner.args` and `runner.mode` apply only to the legacy command and are
rejected alongside processors. Typed processors always invoke one input per task;
custom batch processors retain grouping by schema. Mixed custom modes in one run
are rejected instead of reporting an incorrect common mode.

## Native and frontend contracts

| Type               | Capability probe                                            | Document invocation                                        | Native formats                       |
| ------------------ | ----------------------------------------------------------- | ---------------------------------------------------------- | ------------------------------------ |
| `sourcemeta`       | `jsonschema compile <schema>`                               | `jsonschema validate <schema> <input> --json`              | JSON, YAML                           |
| `check-jsonschema` | frontend `probe <schema>`                                   | frontend `validate <schema> <input>`                       | JSON, YAML, TOML, JSON5, JSONC       |
| `ajv`              | frontend `compile <schema>`                                 | frontend `validate <schema> <input> --compiled <template>` | JSON                                 |
| `custom`           | None; compatibility is the configuration author's assertion | Configured arguments and mode                              | All, or explicit `formats` allowlist |

Sourcemeta uses its documented exit classes: compilation exit 3 is unsupported;
exit 4 rejects the schema for that processor. Both permit selection to continue before
document validation. Exit 4 also covers unknown dialects, vocabularies and compiler
limitations, so it is not proof that every validator rejects the schema. If all candidates
reject it, selection fails (or records an explicit unsupported skip), retaining the rejection
reasons. Other nonzero probe exits are infrastructure/configuration failures. Compilation
stdout is retained as a verified template and passed to validation with `--template`.
Sourcemeta receives an explicit empty tool configuration for both steps: a consuming
project's ambient `jsonschema.json` must not silently change a managed compilation.
No HTTP-enabling flags are added.

The owned frontends return exactly one capability JSON object on stdout:
`{"supported":true}` or `{"supported":false,"reason":"..."}`, with exit 0.
A schema rejection or infrastructure failure exits nonzero instead of pretending to be
an unsupported capability response. Frontends deny remote reference retrieval. Their
document command emits structured JSON, exiting 0 for valid and nonzero for invalid.
A frontend's missing dependency must fail, never request another processor. Probe exit
4 is a schema rejection eligible for prevalidation selection; exit 2 is a fatal usage/configuration
error, and other nonzero frontend probe exits are infrastructure failures.

AJV's `compile` returns the capability object plus a `template` string containing
standalone compiler output. The frontend resolves that artifact's dependencies from
its own installation, not the input project. Its `identity` and the Python frontend's
`identity` report source, runtime and dependency versions for the compilation recipe.
Python has no portable serialized validator here: only its capability check is cached;
the document process still constructs a validator. No pickle or input-generated code
is loaded as a substitute.

## Prepared compiler artifacts

`materialize --compile` prepares the configured processors while the source catalog
is being installed. `--compile-schema` can restrict this to reviewed catalog IDs.
The stored recipe includes schema SHA-256, executable SHA-256, frontend/runtime
identity, explicit processor settings, the compiler recipe version and platform.
Changing any of them creates a new entry. Artifact size and SHA-256 are verified
before use; writes are atomic and locked across processes. Known incompatibilities
are cached, but a compiler crash, timeout or unavailable executable never becomes one.
Publication rechecks a recipe after acquiring its lock and serializes shared artifact
bytes by their digest before installing metadata. Waiting writers keep the first
verified result; cancellation never leaves a partial compilation record.
Sourcemeta's generated file URI for an anonymous schema snapshot is replaced with
a content-derived `urn:schepherd:schema:<sha256>` diagnostic base. This preserves
instance locations and messages without embedding temporary or installation paths
in a relocated compiled artifact. Explicit schema identifiers remain unchanged.

Run against a writable cache can compile an absent entry locally without network.
A complete cache can be relocated and read-only; `materialize --offline --compile`
only verifies it. Local schemas use persistent compilation only when an explicit
cache directory is configured and no external reference can introduce an unhashed
dependency. A custom `$schema`, including one nested in another schema, also keeps
compilation invocation-local: its dialect or vocabulary can change independently
of the root document. Only the built-in standard dialects need no dependency hash.
This check applies to catalog schemas too. Original schema files stay in place.
Every document is still validated on every run; these are compilation caches,
never cached validation results.
Catalog preparation refuses schemas whose dependencies cannot be closed safely;
it never counts an invocation-local temporary template as a prepared artifact.
Use `--compile-schema` to prepare the reviewed safe catalog subset. A literal
property named `$schema` remains ordinary instance data, not a dialect declaration.

Probes and converters have a 30-second timeout. Probe/conversion output is bounded
to 1 MiB per stream; serialized compiler output is bounded to 64 MiB. Compilation
is not document validation and never uses the user's document as a capability test.

Typed presets intentionally have fixed arguments: accepting arbitrary arguments while
probing a different command would falsely claim compatibility. Use `custom` for a
validator with project-specific flags; its compatibility and output declaration are then
explicitly the author's responsibility. The legacy `runner.command` path remains
unchanged and is reported as `custom`/`raw`; executable names are not guessed.

## Explicit yq conversion

There is no automatic conversion. To opt in, bind the exact yq executable and list the
source formats:

```toml
[processors.native]
type = "sourcemeta"
command = "${JSONSCHEMA_BIN}"
convert = "${YQ_BIN}"
formats = ["toml"]
```

Supported format names are `json`, `yaml`, `toml`, `json5`, `jsonc`. yq is invoked as
`yq -p=<format> -o=json -I=0 <original-input>`. That executable must actually support
the requested source format; a conversion failure is not permission to skip validation.
The output must be exactly one strict UTF-8 JSON document, without duplicate keys.
Multiple documents are rejected rather than silently selecting one.

Converted inputs are private temporary files, removed after the run. Machine tasks retain
original input names in `files` and the actual validator inputs in `paths`. Diagnostics
refer to original files but cannot claim source line/column coordinates from converted
JSON. Native YAML inputs can retain multiple documents without conversion.
Environment templates using `{file}` always name the original source document,
including during compilation and after conversion. Argument templates still name
the actual JSON input the validator must read. This keeps input-dependent compiler
settings identical to the validation settings without losing source attribution.

Unknown or extensionless inputs are unsupported for typed processors rather than guessed
as JSON. Use a custom processor if its own parser intentionally owns format detection.
