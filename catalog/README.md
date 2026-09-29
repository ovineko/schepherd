# catalog

`state.json` in this directory is the record of the published schema catalog
(`ghcr.io/ovineko/schepherd-schemas`). It appears with the first publication. For every
schema ID it holds the catalog entry exactly as published (artifact digest
and size, name, description, `fileMatch`, dialect, provenance), the digests
of the schema content and its notice, the license decision it was published
with, the revision that first published it, the revision that last changed
it (and, after a metadata-only change, the one that gave it its artifact),
and why the catalog keeps it without refreshing it (`heldSinceRevision`,
`heldReason`) or no longer lists it (`excludedRevision`). Globally it names
the upstream commit and tarball digest, the prepare recipe and the current
catalog revision, digest and size.

A published schema never leaves the catalog on its own: one that cannot be
refreshed is held at its last published version, and only an explicit
exclude rule in `sources/licenses.toml` removes one, while its record stays
here so the ID remains reserved.

The weekly catalog job (`.github/workflows/update-schemas.yml`) is the only
writer: it commits a new version of this file, signed by GitHub, only when
the published catalog changed, and tags that commit `catalog-<revision>`.
The same commit carries `sources/schemastore.toml` and the regenerated
`THIRD_PARTY_NOTICES.md`, whose section on the catalog's schemas is derived
from this file. Do not edit the file by hand.

The file is in the one canonical form the publisher writes and accepts back;
any other form is refused. `.datamitsuignore` (`catalog/state.json: *`)
keeps every `pnpm dm check` and `pnpm dm fix` formatter and linter off it,
and `.typos.toml` excludes it from typos, because upstream names are not
typos. Never run a formatter on it. editorconfig-checker and gitleaks still
scan it, and the canonical form passes both.

- Format: [`api/state.schema.json`](../api/state.schema.json) and
  [Publishing](../docs/publishing.md#state)
- The weekly job and its setup: [Catalog automation](../docs/automation.md)
- Pinning a catalog in your project: [Versioning](../docs/versioning.md#catalog-revisions)
