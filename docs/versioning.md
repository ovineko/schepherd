# Versioning

One repository carries two independent release trains:

| Train   | What is released                                                                             | Version                                                     | Git tag                 | Started by                                                 | Published to                                                              |
| ------- | -------------------------------------------------------------------------------------------- | ----------------------------------------------------------- | ----------------------- | ---------------------------------------------------------- | ------------------------------------------------------------------------- |
| Client  | `schepherd` binary, npm launcher and platform packages, PyPI wheels, RubyGems gem, Go module | SemVer `X.Y.Z[-prerelease]`, first release `0.1.0`          | `vX.Y.Z[-prerelease]`   | a maintainer pushing the tag                               | GitHub Releases (marked latest), npm, PyPI, RubyGems, the Go module proxy |
| Catalog | a snapshot of the published JSON Schemas                                                     | revision `YYYYMMDD.HHMM`, the UTC minute of the publication | `catalog-YYYYMMDD.HHMM` | the weekly catalog job, only when upstream content changed | the OCI registry, GitHub Releases (never marked latest)                   |

The binary and its packages contain no schemas and no catalog: the client
fetches the catalog you pin from an OCI repository. A catalog depends only on
its `formatVersion` (see
[Format versions](#format-versions-are-a-separate-axis)), not on a client
version. Updating schemas never requires a new client, and a new client never
changes a catalog.

```text
client:  0.1.0            (Git tag v0.1.0)
catalog: 20260930.0300    (Git and OCI tag catalog-20260930.0300)
pin:     registry.example/org/schemas@sha256:<catalog index digest>
```

## Client versions

The client uses strict [Semantic Versioning 2.0.0](https://semver.org/):
`X.Y.Z` for a release and `X.Y.Z-<prerelease>` for a pre-release, without
leading zeros and without build metadata. A pre-release must be `-alpha.N`,
`-beta.N` or `-rc.N` (see [Pre-releases](#pre-releases)). The owner chooses
which part to increase when tagging.

| Where                      | Form                                                       | Example                                           |
| -------------------------- | ---------------------------------------------------------- | ------------------------------------------------- |
| `schepherd version`        | `X.Y.Z[-prerelease]`                                       | `0.1.0`                                           |
| Release archives           | `schepherd_<X.Y.Z>_<os>_<arch>.tar.gz` (`.zip` on Windows) | `schepherd_0.1.0_linux_amd64.tar.gz`              |
| npm packages               | `@ovineko/schepherd@X.Y.Z[-prerelease]`                    | `@ovineko/schepherd@0.1.0`                        |
| PyPI project `schepherd`   | PEP 440: `X.Y.Z`, `X.Y.ZaN`, `X.Y.ZbN`, `X.Y.ZrcN`         | `schepherd==0.1.0`, `schepherd==0.2.0rc1`         |
| RubyGems gem `schepherd`   | `X.Y.Z`, `X.Y.Z.alpha.N`, `X.Y.Z.beta.N`, `X.Y.Z.rc.N`     | `schepherd-0.1.0.gem`, `schepherd-0.2.0.rc.1.gem` |
| Git release tag, Go module | `vX.Y.Z[-prerelease]`                                      | `v0.1.0`, `v0.2.0-rc.1`                           |

`0.0.0` is reserved: snapshot builds are `0.0.0-snapshot-<short commit>` and
never a release, and neither are Go pseudo-versions. Scripts and workflows
parse versions and compute the PyPI and RubyGems forms only with
`go run ./tools/release version …`.

### Pre-releases

A pre-release version is `X.Y.Z-alpha.N`, `X.Y.Z-beta.N` or `X.Y.Z-rc.N` with
`N` of 1 or more: the only forms PyPI and RubyGems can publish in SemVer
order (PEP 440 knows only a, b and rc with one number, and RubyGems ignores a
trailing `.0`). `version verify-tag` and `version parse` refuse every other
pre-release, such as `-rc1`, `-RC.1`, `-preview.1`, `-rc.1.2` or `-rc.0`, and
name the registry that cannot express it.

| Tag              | npm (dist-tag)           | PyPI       | RubyGems        | GitHub                    |
| ---------------- | ------------------------ | ---------- | --------------- | ------------------------- |
| `v0.2.0-alpha.1` | `0.2.0-alpha.1` (`next`) | `0.2.0a1`  | `0.2.0.alpha.1` | pre-release, never latest |
| `v0.2.0-beta.2`  | `0.2.0-beta.2` (`next`)  | `0.2.0b2`  | `0.2.0.beta.2`  | pre-release, never latest |
| `v0.2.0-rc.1`    | `0.2.0-rc.1` (`next`)    | `0.2.0rc1` | `0.2.0.rc.1`    | pre-release, never latest |
| `v0.2.0`         | `0.2.0` (`latest`)       | `0.2.0`    | `0.2.0`         | release, marked latest    |

No channel installs a pre-release by default: ask for one with
`@ovineko/schepherd@next` or an exact npm version, `pip install --pre
schepherd` or `schepherd==0.2.0rc1`, and `gem install schepherd --pre` or
`-v 0.2.0.rc.1`. Homebrew, Scoop and winget receive releases only.

### Go module tags

The release tag is also the Go module version
(`go install github.com/ovineko/schepherd/cmd/schepherd@v0.1.0`). Go ignores
the `catalog-*` tags, and `@latest` is the newest version that is not a
pre-release. A major version 2 or higher needs a `/vN` module path:
`version verify-tag` and `version parse` refuse such a tag until the module
path in `go.mod` ends in `/vN` (checked with `golang.org/x/mod`).
`TestModuleTagInstalls` in `tools/release/internal/gomodule` installs the
client from a tagged local copy of the module, offline, with the real `go`
command, and checks the version the binary reports.

### Binary version

`schepherd version` prints `X.Y.Z[-prerelease]` for a release build and `dev`
otherwise. A build is a release build when GoReleaser injected the tag's
version (`-X github.com/ovineko/schepherd/internal/buildinfo.version=…`), or
when the Go toolchain recorded a release tag as the module version:
`go install …@vX.Y.Z`, or `go build` in a clean checkout of that tag.
Pseudo-versions, `(devel)`, `+dirty` builds and snapshots report `dev`; an
injected snapshot version decides even when the commit carries a release
tag.

```json
{
  "version": "0.1.0",
  "commit": "…",
  "commitDate": "…",
  "goVersion": "go1.27.1",
  "release": true,
  "catalogFormatVersions": [2],
  "configFormatVersions": [1]
}
```

`commit` and `commitDate` are omitted when unknown, as for a `go install`
build.

### Snapshots

CI and `task verify:packaging` build snapshots with the version
`0.0.0-snapshot-<short commit>`, read from GoReleaser's `dist/metadata.json`
after the build with `go run ./tools/release version snapshot --dist dist`.
Their npm packages carry that version, and their binaries report `dev`.

A snapshot can never reach PyPI or RubyGems: its wheels carry the local
version `0.0.0+snapshot.<commit>`, which PyPI refuses; its gem has the
version `0.0.0.1.snapshot.<commit>` and the `allowed_push_host`
`https://snapshot.invalid`; and `packages publish-plan` refuses snapshots for
both.

## Releasing the client

### Release tags

A maintainer releases a commit of `main` by pushing its tag:

```bash
git fetch --tags origin
git tag v0.1.0 <commit on main>
git push origin v0.1.0
```

The release notes are GoReleaser's changelog of the commit subjects since the
previous release tag, grouped by Conventional Commit type (`docs`, `test`,
`ci` and `chore` commits are left out).

The release workflow runs only for tags that match `v[0-9]+.[0-9]+.[0-9]+`
or `v[0-9]+.[0-9]+.[0-9]+-*`; catalog tags never start it. Its first job
checks the tag:

```bash
go run ./tools/release version verify-tag [--repo .] [--base origin/main] [--json] <tag>
```

It accepts the tag only when all of these hold:

- it is a valid release tag with an allowed pre-release form (checked before
  any Git lookup);
- no other tag carries the same version, such as `v0.1.0+build`;
- it is newer than every existing `v*` tag that is not a pre-release
  (`v0.1.1` may follow `v0.2.0-rc.1`);
- its commit is reachable from `--base` (the workflow passes `origin/main`);
- in the tagged commit, the module path in `go.mod` fits the major version
  (`/vN` for major 2 and above, no suffix for `v0` and `v1`).

It prints the version, or with `--json`:

```json
{
  "version": "0.2.0-rc.1",
  "tag": "v0.2.0-rc.1",
  "npmDistTag": "next",
  "pypiVersion": "0.2.0rc1",
  "gemVersion": "0.2.0.rc.1",
  "previousTag": "v0.1.1",
  "prerelease": true
}
```

`previousTag` is the newest lower release tag reachable from the tagged
commit; for a release that is not a pre-release it is the newest lower
release that is not a pre-release either, so the notes of `v0.2.0` cover
everything since `v0.1.1`. It is absent for the first release. The workflow
passes it to GoReleaser as `GORELEASER_PREVIOUS_TAG` and the tag as
`GORELEASER_CURRENT_TAG`, because `catalog-*` tags on `main` must never start
or end a changelog. `verify-tag` exits with 1 when the tag is rejected, is
not a tag of the repository or the base is missing, and with 2 for usage
errors.

The other subcommands:

| Command                                            | Prints                                                                                                                                                                   |
| -------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `version parse <version-or-tag> [--go-mod go.mod]` | the JSON of `verify-tag --json` without `previousTag`; exit 1 for anything that is not a release version or tag, has no PyPI or RubyGems form or does not fit `--go-mod` |
| `version snapshot [--dist dist]`                   | the GoReleaser snapshot version from `<dist>/metadata.json`; exit 1 unless it is `0.0.0-snapshot-<commit>`                                                               |

### Release workflow

`.github/workflows/release.yml` runs on every pushed release tag. Every
publishing job runs in a protected environment of its own, whose required
reviewer approves it before it starts:

| Job                | What it does                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| ------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `tag`              | `version verify-tag --json --base origin/main`; outputs the version, npm dist-tag, PyPI and gem versions and the previous tag.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| `ci`               | Calls the whole CI workflow for the tagged commit; it must pass.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| `release`          | Environment `release`. `goreleaser build --clean` builds the binaries of exactly the tag; `packages build --smoke` builds the npm tarballs, PyPI wheels and gem from them, checks and installs them, and uploads them as the workflow artifact `release-packages` (they are not release assets). `goreleaser release --clean` then builds again and publishes the archives, an SPDX SBOM of every archive, the checksum file and its cosign signature into a draft that is published once complete, with the changelog as notes; for a release (never a pre-release) it also updates `ovineko/homebrew-tap`, `ovineko/scoop-bucket` and opens a winget pull request to `microsoft/winget-pkgs`. The job then requires the second build's binaries to equal the first byte for byte and the release to be published, and `actions/attest` attests the archives, SBOMs, checksum files and packages. |
| `publish-npm`      | Environment `npm`. `packages publish-plan --kind npm` checks the tarballs against their checksum file and asks the registry which it already has; `npm publish --provenance --access public --tag <latest or next>` publishes the rest, platform packages first and the launcher last.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| `publish-pypi`     | Environment `pypi`. `packages publish-plan --kind pypi` checks the wheels and asks PyPI which it already has; `pypa/gh-action-pypi-publish` uploads the others, each with a PEP 740 attestation.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| `publish-rubygems` | Environment `rubygems`. `packages publish-plan --kind gem` checks the gem and asks rubygems.org for the version; when it is missing, the pinned `sigstore-cli` (`tools/pins/sigstore-cli`) signs its attestation, `rubygems/configure-rubygems-credentials` turns the OIDC token into a short-lived API key, and `gem push --attestation` uploads both.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |

The three registry jobs start once the `release` job found the GitHub release
published with the binaries of the packages, even when a Homebrew, Scoop or
winget push failed after that, and do not depend on each other. A version a
registry already has with the same bytes is skipped; one it has with other
contents fails the job. How to verify what a release published is described
in [Verify a release](installation.md#verify-a-release).

**Trusted publishing.** `publish-npm`, `publish-pypi` and `publish-rubygems`
hold no token: each registry exchanges the job's GitHub OIDC token for a
short-lived credential, but only for its own package, the repository
`ovineko/schepherd`, the workflow file `release.yml` and the job's
environment (`npm`, `pypi`, `rubygems`). Renaming the workflow file or an
environment stops that registry's publication until its trusted publishers
are configured again; on npm each of the seven packages (`@ovineko/schepherd`
and `@ovineko/schepherd-<platform>` for `linux-x64`, `linux-arm64`,
`darwin-x64`, `darwin-arm64`, `win32-x64` and `win32-arm64`) has its own.
npm trusted publishing needs npm CLI 11.5.1 or later and Node.js 22.14.0 or
later (the job uses `.node-version`), and npm accepts provenance only from a
public repository. The gem sets `rubygems_mfa_required`, which applies to
manual pushes, not to trusted publishing.

Before the first release, a repository administrator:

1. creates the environments `release`, `npm`, `pypi` and `rubygems`, each
   with a required reviewer and deployments allowed only from tags that
   match `v*`, and enables release immutability (Settings, General,
   Releases);
2. configures the trusted publisher of each npm package on npmjs.com
   (package settings, Trusted publishing, GitHub Actions: organization
   `ovineko`, repository `schepherd`, workflow `release.yml`, environment
   `npm`) and allows it to run `npm publish`; npm offers this only for a
   package that already exists;
3. shortly before the first release, adds a pending publisher on pypi.org
   (account menu, Publishing, GitHub: project `schepherd`, owner `ovineko`,
   repository `schepherd`, workflow `release.yml`, environment `pypi`) and on
   rubygems.org (profile, OIDC, Pending trusted publishers: gem `schepherd`,
   repository `ovineko/schepherd`, workflow `release.yml`, environment
   `rubygems`). A pending publisher creates the package on its first upload,
   does not reserve the name and expires if unused (on PyPI after 30 days,
   on RubyGems after 12 hours);
4. makes the organization secrets `HOMEBREW_TAP_TOKEN`, `SCOOP_TOKEN` and
   `WINGET_TOKEN` available to this repository: tokens that may write to
   `ovineko/homebrew-tap` (directory `Casks`), `ovineko/scoop-bucket` and the
   fork `ovineko/winget-pkgs` and open pull requests on
   `microsoft/winget-pkgs`.

No repository variable switches publication on or off: the environments
decide. A reviewer who rejects a deployment stops that job alone.

**Finishing an interrupted release.** "Re-run failed jobs" finishes a run
with the packages it built: each registry job publishes only what its
registry does not have yet. The GitHub release is immutable once published;
an interrupted GoReleaser upload leaves at most an unpublished draft, which a
re-run does not reuse (delete it by hand). A re-run after the release was
published builds the packages again (the builds are reproducible), fails only
in GoReleaser's release step, and the registry jobs run as above. A Homebrew,
Scoop or winget push that failed after publication is finished by hand: in a
checkout of the tag,
`GORELEASER_CURRENT_TAG=<tag> .tools/bin/goreleaser release --clean --skip=publish,sign,sbom`
writes the manifests with the checksums of the reproducible archives to
`dist/homebrew`, `dist/scoop` and `dist/winget`.

### Documentation website

`.github/workflows/deploy-website.yml` builds the documentation from `docs/`
with `pnpm docs:build` (`zensical build --strict`, output in `dist/website`)
and deploys it to GitHub Pages on every pushed stable release tag `vX.Y.Z`
and on a manual run, so <https://schepherd.ovineko.com> documents the latest
release. A manual run deploys the branch or tag it was started from, for
example a documentation fix ahead of the next release:

```bash
gh workflow run deploy-website.yml --ref vX.Y.Z
```

To enable the website, a repository administrator:

1. selects GitHub Actions as the source in Settings, Pages (Pages needs a
   public repository on GitHub Free);
2. enters the custom domain `schepherd.ovineko.com` there first, and only then
   creates the DNS record `schepherd.ovineko.com CNAME ovineko.github.io`, so
   nobody else can claim the subdomain in between; verifying the domain for
   the organization keeps other accounts from using it. A site deployed by a
   workflow ignores a `CNAME` file, so the repository has none;
3. enables Enforce HTTPS once GitHub has issued the certificate;
4. sets Deployment branches and tags of the environment `github-pages` to No
   restriction, so release tags and manual runs from any branch can deploy.

## Catalog revisions

A catalog revision is the UTC minute of its publication, written
`YYYYMMDD.HHMM` with zero-padded hours and minutes, for example
`20260924.0905`. The date is a real calendar date from 2000 to 9999. There is
no counter: the fixed width makes the textual order of revisions their
chronological order. Clients refuse a catalog with any other revision form as
an invalid artifact (exit code 5).

| Tag                     | Where                 | Points to                                                              |
| ----------------------- | --------------------- | ---------------------------------------------------------------------- |
| `catalog-YYYYMMDD.HHMM` | OCI registry          | the catalog index of that revision; never moves                        |
| `catalog-latest`        | OCI registry          | the newest catalog; the only mutable tag, read only by `schepherd pin` |
| `catalog-YYYYMMDD.HHMM` | Git (this repository) | the commit that recorded the revision in `catalog/state.json`          |

Schema artifacts have no tags: the catalog index references them (see
[Retention](mirroring.md#retention)).

Every catalog revision also gets a GitHub release `Schemas <revision>`, never
marked latest, so the repository's latest release is always the newest
client. A snapshot's identity is its **catalog index digest**, not its tag:
runtime commands work only with the digest.

One revision never names two catalogs: a publication in a minute that already
has a catalog with other content, or with a clock behind the newest revision,
is refused, and an interrupted publication with identical content is resumed
under its original revision (see [Publishing](publishing.md#publishing)).
When nothing changed, the weekly job creates no revision at all; the time of
the last check appears only in its job summary.

## Format versions are a separate axis

| Axis                    | Example                | Changes when                                  |
| ----------------------- | ---------------------- | --------------------------------------------- |
| client version (SemVer) | `0.1.0`                | a client release is published                 |
| catalog revision        | `20260930.0300`        | a catalog is published                        |
| catalog `formatVersion` | `2`                    | the catalog document changes incompatibly     |
| config `config_version` | `1`                    | the configuration format changes incompatibly |
| state `formatVersion`   | `1`                    | the publisher state file changes incompatibly |
| media type suffix `v2`  | `…schepherd.schema.v2` | the artifact wire format changes incompatibly |
| JSON Schema dialect     | draft-07, 2020-12      | chosen by each upstream schema                |
| digests                 | `sha256:…`             | content changes                               |

An incompatible format change gets a new format number, and older clients
refuse it explicitly with exit code 2 and a message saying the client is too
old: a catalog `formatVersion` other than 2, a `config_version` other than
1, or a Schepherd media type whose wire format suffix is newer than `v2`
(see [OCI format](oci-format.md)). Wire format 2 made the catalog an OCI
image index over its schema artifacts; version 1 was never published, and
its artifacts are refused as invalid. Nothing is inferred from client versions or
revision dates.
