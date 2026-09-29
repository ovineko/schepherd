# Mirroring and retention

`schepherd mirror` copies one complete catalog snapshot into another OCI
repository, byte for byte. Afterwards you change only the repository in your
configuration; the catalog digest stays exactly the same.

```bash
schepherd mirror \
  registry.example/org/schemas@sha256:<catalog-digest> \
  mirror.internal.example/team/schemas --json
```

```json
{
  "source": "registry.example/org/schemas",
  "destination": "mirror.internal.example/team/schemas",
  "catalogDigest": "sha256:…",
  "revision": "20260930.0300",
  "schemas": 412,
  "copiedManifests": 414,
  "skippedManifests": 0,
  "copiedBlobs": 415,
  "skippedBlobs": 0,
  "tagsCreated": 1,
  "tagsExisting": 0
}
```

The counters describe one run (the example: 412 schemas that share the empty
config and one notice, mirrored into an empty repository):

| Counter            | Counts                                                                                                                   |
| ------------------ | ------------------------------------------------------------------------------------------------------------------------ |
| `schemas`          | distinct schema manifests of the catalog (the schema children of the index)                                              |
| `copiedManifests`  | manifests uploaded, including the catalog index and the catalog metadata manifest                                        |
| `skippedManifests` | manifests already present in the destination and therefore not traversed; a repeated mirror reports exactly 1, the index |
| `copiedBlobs`      | distinct blobs uploaded (payloads, notices, config, catalog document)                                                    |
| `skippedBlobs`     | distinct blobs already present in the destination                                                                        |
| `tagsCreated`      | 1 when the run created the `catalog-<revision>` tag, otherwise 0                                                         |
| `tagsExisting`     | 1 when `catalog-<revision>` already pointed to the catalog index, otherwise 0                                            |

A blob shared by many schema artifacts is uploaded once per run.

## Generic OCI tools

A catalog is an OCI image index whose children are its metadata manifest and
every schema manifest it lists (see [OCI format](oci-format.md#catalog-index)),
so the whole snapshot is one OCI graph. Any generic copy of the index, such as
`oras cp`, `crane copy`, `regctl image copy` or `oras.Copy` in Go with default
options, copies the complete snapshot byte for byte, and the pinned catalog
digest keeps working against the copy (scenario E42 checks this with plain
ORAS).

## Why `schepherd mirror`

`schepherd mirror` is still the recommended way to copy a catalog because it
verifies everything it copies. A generic copier trusts the source registry:
ORAS, for example, streams a source response into the destination and checks
at most its headers. `schepherd mirror` refuses a snapshot a client would
refuse, before and while it is copied.

## Algorithm

1. Fetch the source catalog index, its metadata manifest and `catalog.json`
   and verify them exactly as the client does, including the check that
   `catalog.json` lists exactly the schema children of the index. The
   verified bytes are reused for the copy.
2. Copy the graph of the index with ORAS, children before parents and with
   bounded concurrency (`--concurrency`, default 4, 1 to 64 manifests and
   blobs at a time). Every schema manifest is checked against the wire
   contract before it is copied, and every body streamed from the source is
   verified against its descriptor's size and digest while it is uploaded
   (see [What a pin guarantees](security.md#what-a-pin-guarantees)). A
   manifest that the destination already has, the index included, is taken
   to have its whole graph and is skipped with everything it references.
3. Only after the index exists, ensure the `catalog-<revision>` tag.

The index is uploaded last, so an interrupted mirror never leaves an index or
a catalog tag pointing at an incomplete snapshot; re-running it continues
where it stopped. Interrupting the command (Ctrl-C or a cancelled run) exits
with code 130, whether it happens during a blob upload or a manifest push.
The command never deletes anything in the destination and never moves an
existing tag: a `catalog-<revision>` tag that already points elsewhere is an
error. It does not create or move `catalog-latest`.

Data is streamed through the client; nothing is transferred
registry-to-registry directly, and schemas are never all held in memory at
once. A corrupt, truncated or overlong source body never completes an
upload: the mirror stops with exit code 5 and names the bad digest, no
catalog tag is created, and re-running after the source is repaired
completes the mirror.

Source and destination use the registry settings of their own entries in
`[registries]`. An entry can name a host or a repository path prefix, so a
mirror between two repositories on the same host (for example
`ghcr.io/org-a/schemas` to `ghcr.io/org-b/schemas`) can use different
accounts, CA files and `plain_http` settings. Give each side its own entry
(see [Registries and credentials](configuration.md#registries-and-credentials)):

```toml
[registries."ghcr.io/org-a"]
credentials_file = "./org-a.json"

[registries."ghcr.io/org-b"]
credentials_file = "./org-b.json"
```

`mirror` does not use the `[catalog]` section of the configuration: the source
and its catalog digest come from the first argument, the destination from the
second. It ignores local `[schemas]` as well: they are files of your project,
never registry content.

## Retention

Registries keep content alive through tags and through the references of
tagged content. The only tags are `catalog-<revision>` (created by the
publisher and by `mirror`) and `catalog-latest` (moved by the publisher
only), and both point at a catalog index. The index references the metadata
manifest and every schema manifest, which reference their blobs, so a tagged
index keeps its whole snapshot alive, also in registries whose garbage
collector removes untagged manifests.

Mirroring a newer snapshot into a destination that already holds an older
one never moves or deletes the older snapshot's tag. The older catalog and
every schema it lists keep resolving by digest there.

Deleting a `catalog-<revision>` tag makes its snapshot collectable once no
other tagged index references it. Some registries list the children of an
index as untagged versions: GHCR shows every schema manifest and the
metadata manifest as an untagged package version. Never apply a cleanup
policy that deletes untagged versions (a "delete untagged" action or
retention rule) to a schemas repository; it breaks pins although their
catalog tags still exist. A digest is only as durable as the registry that
stores it: an administrator can delete content. Schepherd promises
verifiable identity of whatever is available, not eternal availability. For
long-term vendoring use `schepherd export`.

## After mirroring

1. Point `catalog.repository` (or `SCHEPHERD_REPOSITORY`) at the mirror. Keep
   `catalog.digest`.
2. The client resolves every schema inside the mirror repository. Nothing
   refers back to the source registry or to upstream URLs, so the source can
   be switched off.

## Pull-through proxies

A pull-through proxy (for example a registry's dependency proxy feature) is
not required and not the baseline. Whether a given proxy works depends on its
upstream routing and on whether it serves OCI artifact manifests with custom
media types by digest, not merely on `@sha256` support. The supported and
tested paths are an explicit `schepherd mirror`, or a generic copy of the
catalog index, into a normal OCI registry.
