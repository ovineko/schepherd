# Security and trust model

## What a pin guarantees

A configuration pins `repository` and the **catalog index digest**. Every
byte Schepherd uses is checked against that pin:

1. the catalog index must hash to the pinned digest, within
   `limits.max_manifest_bytes`;
2. the catalog metadata manifest must match its descriptor in the index, and
   `catalog.json` the layer descriptor in that manifest;
3. the schema manifests `catalog.json` lists must be exactly the schema
   children of the index (digests, sizes and media types);
4. each schema manifest must match the descriptor listed in the catalog;
5. each payload blob must match its descriptor;
6. the decompressed schema must match the content digest and size recorded in
   the schema manifest.

HTTP headers such as `Docker-Content-Digest` or `Content-Length` are never
trusted on their own. Every read is bounded by the configured limits, and
gzip payloads are decompressed only up to the declared size.

`schepherd mirror` checks what it copies the same way. The index, the
metadata manifest and `catalog.json` are verified as above before anything
is copied, every schema manifest is checked against the wire contract, and
every blob streamed from the source is verified against its descriptor's
digest and size while it is uploaded. The last bytes are held back until the check passes, so even a
destination registry that does not verify uploads never receives a complete
upload of wrong content; the mirror stops with exit code 5 before any catalog
tag is created (see [Mirroring](mirroring.md)).

A digest proves that the content equals what the pin refers to. It does not
prove who published it and it is not a signature. Distribute the pin itself
through a trusted, reviewed channel (for example a configuration file in your
repository). Who published a catalog is attested separately (see below), and
so is every client release (see
[Verify a release](installation.md#verify-a-release)).

### Verifying a published catalog

Every catalog the weekly job publishes has a
[build provenance attestation](https://docs.github.com/en/actions/concepts/security/artifact-attestations)
of its index digest, signed keyless with the identity of
`update-schemas.yml` on `main` and stored both by GitHub and in the registry
next to the catalog. Because the catalog index references every schema
artifact by digest, the attestation covers them all. With the digest your
configuration pins (or the one `schepherd pin` prints):

```bash
gh attestation verify oci://ghcr.io/ovineko/schepherd-schemas@sha256:<catalog index digest> --repo ovineko/schepherd \
  --signer-workflow ovineko/schepherd/.github/workflows/update-schemas.yml
```

`--bundle-from-oci` reads the attestation from the registry instead of
GitHub's API. The client itself never needs the attestation: it verifies
everything against the pinned digest.

## Local cache

- Cache paths are derived only from validated digests. Annotations such as
  `org.opencontainers.image.title` are ignored; the materialized file is
  always `schema.json`.
- All cache writes go through `os.Root`, so a symlink or `..` inside the cache
  cannot redirect a write outside it.
- On Windows, directory junctions, which any user can create without
  privilege, are refused like symlinks. Go reports them as irregular files,
  and the cache treats every entry that is not a regular file or a real
  directory as corrupt: it is refused offline and quarantined and rebuilt
  online. `os.Root` refuses to follow links whose target is absolute or
  outside the cache. A symlink at a lock file path is replaced by a real lock
  file; a junction there is refused with an error naming the path, and must be
  removed by hand. A linked layout directory makes the cache unusable (exit
  code 2), and the error names the path.
- Downloads land in a temporary file on the same filesystem and are renamed
  into place only after verification. Incomplete data is never visible under
  a final name.
- Concurrent processes coordinate with per-digest file locks.
- Every `path`, `cat` and `export` re-verifies the schema manifest and the
  materialized file. With network access a corrupt entry is quarantined and
  fetched again; with `--offline` it is an error. A cached index or manifest
  of a newer wire format (written by a newer client sharing the cache) is not
  corrupt: it is reported as unsupported (exit code 2) and left in place.
- Once the schema manifest and the materialized file verify, the payload blob
  they were built from is not needed and not read: a missing or damaged
  payload blob causes no request and does not fail `--offline`. It is
  repaired (online) or refused (offline) only when the materialized file has
  to be rebuilt. The notice layer is read only by `export`, under the same
  rules as every other cache entry.
- Quarantined entries (`v1/quarantine` below the cache root) are never read
  again. They are kept for 24 hours so that the damage can be inspected, then
  deleted the next time a command opens the cache.
- Materialized files are read-only, which is a convenience, not a security
  boundary; verification is.
- There is no automatic garbage collection of verified entries, so a path printed by `path` is not
  removed behind a consumer's back. You or your OS may delete the cache at any
  time; use `export` for long-lived copies.

## Network

- HTTPS by default, TLS 1.2 or newer, system roots plus an optional
  `ca_file` per registry entry (a host or a repository path prefix, see
  [Registries](configuration.md#registries-and-credentials)). There is no
  switch to disable certificate verification.
- Plain HTTP only for the repositories of an entry explicitly marked
  `plain_http = true`; there is no automatic HTTPS-to-HTTP fallback.
- Credentials come from Docker-compatible configuration and credential
  helpers, are looked up only after a registry demands authentication, are
  sent only to the host they belong to, and are dropped on redirects to other
  hosts. Two registry entries never share credentials or cached tokens, even
  on the same host. Credentials never appear in output, reports or debug
  messages.
- Every registry, token-service and storage URL in an error message is shown
  with scheme, host and path only; the user information, query and fragment
  are dropped. So neither the time-limited signature of a presigned storage
  URL that a registry redirects a blob or manifest download to (S3
  `X-Amz-Signature`, Azure SAS `sig`, GCS signed URLs) nor an upload
  session's state token reaches stderr or CI logs, also with `--quiet`. A
  refused HTTPS-to-HTTP redirect names its target the same way.
- Retries are bounded: at most 5 per request for timeouts and 408, 429 and 5xx
  responses, with exponential backoff, and at most 3 downloads of a payload
  whose transfer broke off mid-stream. They respect `--timeout` and
  cancellation. Refused connections, authentication and authorization
  failures and integrity failures are not retried.
- `--offline` forbids all network access by Schepherd: no registry requests,
  no token requests, no credential helper runs. It does not isolate the
  consumer you run through `run`; sandbox that separately if needed.
- There is no telemetry and no update check.
- Provenance URLs in the catalog are informational and never contacted.

## Running consumers

- The runner configuration is trusted code: it decides what executes. It is
  loaded only from `--config` or `SCHEPHERD_CONFIG`, never discovered in the
  current directory, and cannot be extended from remote locations.
- Consumers are started with a direct argument vector. No shell, no `eval`, no
  glob or tilde expansion, no word splitting. Input paths are passed as
  absolute paths, so a file named `--help` stays a file.
- Bare command names are resolved in the consumer's own `PATH`, never in the
  current directory.
- Each consumer runs in its own process group (Unix) or job object (Windows)
  that is killed on timeout, interruption and exit. On Unix a descendant that
  starts its own session or group, and every consumer of a Schepherd that was
  killed with `SIGKILL`, escapes this cleanup; see
  [Limits of process cleanup](runner.md#limits-of-process-cleanup).
- On Windows a command that resolves to a `.bat` or `.cmd` file is refused,
  because `cmd.exe` would parse the command line again.

## Local schemas

- [Local schemas](configuration.md#local-schemas) come from the trusted
  configuration, like the runner, and are read only from the local disk: a
  `path` that looks like a URL is refused, so the client never fetches a
  schema from anywhere but its registries.
- They are your own files, used in place, and are not verified against any
  digest. Every command that uses one checks that it is a regular file (a
  symlink is followed), at most `limits.max_schema_bytes`, and well-formed
  JSON without duplicate object keys; it is never copied into the cache, and
  a command that uses only local schemas neither opens the cache nor
  contacts a registry.
- A local schema with the ID of a catalog entry replaces that entry, patterns
  included. Whoever can change the configuration decides which schema a file
  gets, as with `[[mappings]]`.

## Publisher

- Upstream data is fetched only by the maintainer-side publisher, never by
  the client.
- The publisher's HTTP client refuses loopback, private, link-local and other
  internal addresses (checked on the dialed IP, which defeats DNS rebinding),
  re-checks every redirect, refuses HTTPS-to-HTTP downgrades and bounds sizes,
  counts and depth of dependency fetching.
- Nothing from upstream is executed. The bundler runs as a separate process
  without network access, and its processes (the version probe, `inspect`
  and `bundle`) start without `GITHUB_TOKEN` in their environment, so the
  token meant for license detection never reaches a program that reads
  untrusted schemas.
- A publicly reachable URL is not a license to redistribute. Every source
  needs an explicit license decision; unknown sources are held for review and
  are not published. See [Publishing](publishing.md).
- Publishing credentials are available only to the publish job, after
  preparation and tests have finished without them.

### License detection

See [Automatic license detection](publishing.md#automatic-license-detection)
for the rules.

- Only the publisher contacts the detection services (`api.github.com`,
  `registry.npmjs.org`, `unpkg.com`, `cdn.jsdelivr.net`, narrowed by the
  policy's `[auto]` hosts), through the same SSRF-hardened fetcher: public
  addresses only, bounded bodies (2 MiB), no proxies or cookies. Redirects
  may only lead to those services.
- `GITHUB_TOKEN` is sent only to `api.github.com` and dropped on a redirect to
  another host.
- Files read through the GitHub API are checked against the size and Git
  blob ID GitHub reports; npm metadata must name the requested package and
  version. License and `NOTICE` texts with control or Unicode bidirectional
  characters are refused.
- Detection never allows on failure: every problem, rate limits included,
  leaves the source pending review, or holds a published schema at its last
  published version. A recorded decision is reused only while the source and
  every dependency keep their recorded digests, and only after the current
  policy has decided on it again.
- Explicit `exclude` and `review` rules always take precedence over a
  detected license, and on GitHub hosts they match the owner and repository
  in any letter case, so an alias spelling cannot slip past them.
- A detected license always travels with the schema in the artifact's notice
  layer.

### Catalog bot

- Secrets exist only in the publish job of `update-schemas.yml`, in the
  protected `schemas-publish` environment, which alone gets `packages:
write`, `attestations: write` and `id-token: write`. The prepare job uses
  only its read-only `GITHUB_TOKEN` (status lookups and license detection)
  and resolves `catalog-latest` anonymously; the test job's token has only
  `packages: read`. Registry credential files are written with mode 0600 and
  removed at the end of the job, also on failure.
- The GitHub App's private key produces a short-lived installation token,
  limited to `contents: write` on this repository and revoked when the job
  ends.
- Bot commits are created through the GraphQL `createCommitOnBranch`
  mutation and signed by GitHub. A commit refuses to overwrite changes made
  to the files it writes since the run's starting commit.
- `catalogbot` sends its token only to HTTPS endpoints (plain HTTP only to
  loopback addresses, for tests) and redacts it from all output.
- Published tags never move, in Git as in the registry.
- Upstream schema names appear in release notes only inside code spans, so
  they cannot inject Markdown, HTML, `@mentions` or `#references`.

### Release and website workflows

- Every job with a write permission (the OIDC token included) or a secret
  runs in a protected environment: `release`, `npm`, `pypi` and `rubygems`
  with a required reviewer and deployments only from tags `v*`,
  `schemas-publish` only from `main`, `github-pages` for the website. No
  workflow that publishes runs on pull requests, and no repository variable
  switches publication on; the [workflow policy](testing.md#workflow-policy)
  test enforces this.
- The `tag` and `ci` jobs of the release workflow only read the repository.
- Signatures and attestations are made keyless with the publishing jobs'
  OIDC tokens, whose certificates name the workflow and the tag (see
  [Verify a release](installation.md#verify-a-release)). No long-lived
  signing key exists.
- The only secrets are `HOMEBREW_TAP_TOKEN`, `SCOOP_TOKEN` and
  `WINGET_TOKEN`. Only the GoReleaser step of the `release` job names them,
  and GoReleaser uses them only for a release, never a pre-release.
- npm, PyPI and RubyGems are reached only through trusted publishing: each
  registry accepts the job's OIDC token only for its own package, this
  repository, the workflow `release.yml` and that job's environment.
- The GitHub release is published only after every asset is uploaded and is
  immutable afterwards. The registry jobs upload only the packages the
  `release` job built and recorded in a checksum file, after checking them
  against it again, and a version a registry already has with other contents
  fails the job instead of being replaced.
- The website workflow holds no secrets; its deploy job gets only `pages:
write` and `id-token: write`.

## Reporting vulnerabilities

See [SECURITY.md](https://github.com/ovineko/schepherd/blob/main/SECURITY.md).
