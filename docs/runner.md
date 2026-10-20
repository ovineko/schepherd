# Runner

`schepherd run` is a thin adapter: it resolves schemas for the files you pass,
materializes them (or checks [local schemas](configuration.md#local-schemas)
in place), and starts the configured consumer with a direct argument
vector, without a shell. Legacy runners forward the consumer's output unchanged;
[processors](processors.md) declare an output format and may use an explicitly
configured converter. Schepherd does not parse validator findings in Go.

## Planning (before anything starts)

1. Every input path is made absolute (relative to the current directory) and
   checked: it must exist, be a regular file and be readable. Duplicate paths
   are dropped after their first occurrence.
2. Each input gets a schema ID: `--schema <id>` for all inputs, otherwise the
   local `[[mappings]]`, then the `file_match` of local `[schemas]`, then
   catalog `fileMatch` (see [matching](matching.md)). Unmatched inputs fail
   the run unless `--ignore-unmatched` is set; ambiguous inputs always fail.
3. Every needed catalog schema is materialized. Every local schema is checked
   in place (an existing regular file, readable, within
   `limits.max_schema_bytes`, valid JSON) and handed to the consumer as its
   own file, never copied. In batch mode local schemas are grouped by ID like
   catalog schemas.
4. `command`, `args`, `cwd` and `env` are interpolated for every task and the
   executable is resolved (see
   [Executable resolution](configuration.md#executable-resolution)).
   `command` and `cwd` must not expand to an empty string.

Any error in these steps is reported before the first consumer starts.

A run whose files all use local schemas contacts no registry and creates no cache
by default. Typed processors store compiler artifacts only when a cache directory
was explicitly configured; legacy runners do not open it. `{cache}` still expands
to its path; when no cache directory can be determined at all, such a run fails
only if the runner uses `{cache}`.

## Modes

| Mode       | Processes                                                                                   | Input delivery                                             |
| ---------- | ------------------------------------------------------------------------------------------- | ---------------------------------------------------------- |
| `batch`    | one per schema ID (split into chunks if argv and environment would exceed `max_args_bytes`) | `{files...}` expands to one argument per file              |
| `per-file` | one per input                                                                               | `{file}`                                                   |
| `stdin`    | one per input                                                                               | the input's raw bytes are streamed to the consumer's stdin |

Tasks run in a deterministic order. In `batch` mode schema groups run in order
of the first input that selected them, with files in input order. In
`per-file` and `stdin` mode tasks follow input order. In `stdin` mode exactly one input is
streamed per process; files are never concatenated and the schema stays a
file (`{schema}`). In the other modes the consumer's stdin is the null device.

## Execution

- `jobs = 1` (default): consumers run one at a time and inherit Schepherd's
  stdout and stderr directly, unmodified.
- `jobs > 1`: up to `jobs` consumers run at once. Each consumer's stdout and
  stderr are spooled to temporary files (in a `schepherd-output-*` directory
  under the system temporary directory, removed at the end; a killed Schepherd
  can leave it behind) and replayed in task order after it finishes, so output
  is never interleaved.
- `fail_fast = false` (default): every task runs. `fail_fast = true`: after
  the first failure no new task starts; tasks already running finish.
- Each consumer runs in its own process group (Unix) or job object (Windows).
  When a consumer times out or Schepherd is interrupted, the group receives
  `SIGTERM` and, 2 seconds later, `SIGKILL` (on Windows the job is terminated
  at once). After every consumer exits, whatever is still running in its
  group or job is killed, so background processes it left behind do not
  outlive it.

### Limits of process cleanup

On Windows the job object is created with kill-on-close, and every process
the consumer starts belongs to it, so descendants are killed even when
Schepherd itself dies. The consumer is assigned to the job right after it has
started; a child it starts before that moment is not in the job.

On Linux and macOS a process group is a weaker boundary:

- A descendant that leaves the group, by calling `setsid` or `setpgid` as
  daemons and some process supervisors do, is not reached and keeps running.
- If Schepherd itself is killed with `SIGKILL` or crashes, it cannot signal
  the group: the running consumers and their children keep running.
  `SIGINT` and `SIGTERM` are handled and clean up as described above.
- On Linux, Schepherd waits for the consumer to exit without reaping it, so
  the group ID cannot be reused before the group is killed. On macOS and
  other Unix systems the group is killed right after the consumer is reaped.

Processes that a consumer asks a system service to start are never its
descendants on any platform. When a consumer must not leave anything behind
in all cases, run it inside a container or another sandbox with its own
lifetime.

## Exit status

The result is the status of the first failed task **in task order**, not in
completion order:

| Situation                                   | Status    |
| ------------------------------------------- | --------- |
| every consumer exited 0                     | 0         |
| consumer exited with `n`                    | `n`       |
| consumer killed by signal `s`               | `128 + s` |
| consumer exceeded `runner.timeout`          | 124       |
| executable missing, not executable, bad cwd | 7         |
| command resolves to `.bat`/`.cmd` (Windows) | 7         |
| interrupted (SIGINT/SIGTERM)                | 130       |

## Report

`--report <file>` writes a JSON report without touching stdout:

```json
{
  "reportVersion": 1,
  "mode": "batch",
  "exitCode": 1,
  "tasks": [
    {
      "schemaId": "package",
      "schemaRef": "registry.example/org/schemas@sha256:…",
      "origin": "catalog",
      "files": ["/abs/package.json"],
      "status": "failed",
      "exitCode": 1,
      "durationMs": 42
    }
  ],
  "skipped": [{ "file": "/abs/README.md", "reason": "unmatched" }]
}
```

`origin` is `catalog` for a schema of the pinned catalog and `local` for one
from `[schemas]`, whose `schemaRef` is `local:<path relative to the workspace>`
(or `local:<absolute path>` outside the workspace). It says where the task's
schema comes from, not which rule matched a file; use `resolve` for that.

`status` is one of `ok`, `failed`, `timeout`, `start-error`, `canceled`,
`not-started`. When step 4 of planning fails (interpolation, executable or
working-directory resolution, argv budget), the report is still written: it
lists every task that would have run, with status `start-error` when the
consumer cannot be started (missing or non-executable command, unusable working
directory, exit code 7) and `not-started` otherwise. Failures in steps 1–3
(unreadable inputs, unmatched or ambiguous files, unknown schemas,
materialization, an unusable local schema file with exit code 2) exit before
a report is written. The report never contains environment values or
credentials.

## Attributed JSON output

`--json` replaces consumer stdout and stderr with exactly one `reportVersion: 2`
document on stdout, on success and on failure. Schepherd's own logs are suppressed
in this mode. The process still returns the real run exit code. The schema is
[`api/report.schema.json`](https://github.com/ovineko/schepherd/blob/main/api/report.schema.json).
Without this flag, execution and the version 1 file report are unchanged.
Combining it with `--report <file>` writes version 1 to that file and version 2
to stdout.

```json
{
  "reportVersion": 2,
  "workspace": "/abs/project",
  "mode": "per-file",
  "exitCode": 1,
  "tasks": [
    {
      "schemaId": "package",
      "schemaRef": "local:schemas/package.json",
      "origin": "local",
      "files": ["/abs/project/package.json"],
      "paths": ["/abs/project/package.json"],
      "cwd": "/abs/project",
      "processor": {
        "name": "primary",
        "type": "sourcemeta",
        "output": "sourcemeta-json",
        "mode": "per-file"
      },
      "status": "failed",
      "exitCode": 1,
      "durationMs": 42,
      "stdout": { "bytes": 0, "text": "", "truncated": false },
      "stderr": { "bytes": 18, "text": "validation failed\n", "truncated": false }
    }
  ],
  "skipped": []
}
```

Machine mode starts one consumer task per original input. A batch runner's
single `{files...}` argument becomes `{file}` and the actual reported mode is
`per-file`; stdin mode remains stdin. This gives anonymous validator findings
one known input, rather than guessing that the first returned document belongs
to the first filename. A multi-document YAML input is still one input.

`files` contains absolute original inputs; `paths` contains the actual paths
passed or streamed to the consumer. They differ after explicit conversion.
`cwd` is the resolved working directory, and `processor` names the declaration,
type, output format and actual task mode. Legacy commands are described as
`runner` / `custom` / `raw`, never guessed from their executable name. No
executable arguments or environment values are serialized.

Each stream retains at most 256 KiB and all streams share a 4 MiB retention
budget. Consumers are still drained after those limits: `bytes` counts every
original byte read, including discarded bytes, and `truncated` marks discarded
bytes. If a pipe cannot finish within the cleanup grace, the task fails and
both streams are marked truncated; bytes the pipe never delivered cannot be counted.
Valid UTF-8 is stored in `text`; otherwise the retained bytes use `base64`,
including when the retention boundary splits a character or an invalid byte
occurs in the discarded suffix. Exactly one encoding is present. A nonzero,
timed-out or interrupted task never becomes successful because its output was
unrecognized or truncated. The shared budget is divided equally across the two
streams of every planned task, capped at 256 KiB per stream, before execution.
Unused reservations are not reassigned, so retention is independent of parallel
completion order and task order stays stable.

Argument, configuration, input, matching, materialization and planning failures
also produce a document, with a classified `error` (`kind`, `message`, and a
file or files only when known). Tasks are empty when there is no complete
execution plan; no working directory or invocation is invented. `workspace`
is empty if configuration could not be loaded. Execution uses the same status
names as version 1, with empty streams for tasks that could not start. A
`not-started` task has exit code -1; timeout and cancellation remain 124 and 130. Skipped inputs are always listed with their reason, including an explicitly
configured unsupported-format skip.
