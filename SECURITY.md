# Security policy

## Supported versions

Only the most recent client release receives fixes: the newest SemVer tag
`vX.Y.Z`, which GitHub marks latest and npm serves under the dist-tag
`latest` (see [Versioning](docs/versioning.md#client-versions)). Catalog
snapshots are data; a problem in a snapshot is fixed by publishing a new
snapshot, never by changing a published one.

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub's
[private vulnerability reporting](https://github.com/ovineko/schepherd/security/advisories/new)
for this repository. Do not open a public issue for security problems.

Include the Schepherd version (`schepherd version --json`), the command you
ran, and a minimal reproduction. Never include real credentials or tokens.

The [trust model](docs/security.md) describes what Schepherd does and does not
protect against.
