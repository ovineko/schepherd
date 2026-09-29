# Installation

The client is one native binary, `schepherd` (`schepherd.exe` on Windows),
built with `CGO_ENABLED=0` and `-trimpath`. It needs no Docker, no ORAS CLI
and no particular validator. The release archives, npm packages, PyPI wheels
and the gem contain no schemas and no catalogs: those come from the OCI
repository you configure.

Every channel carries the same binaries, built once from the tagged commit
of [github.com/ovineko/schepherd](https://github.com/ovineko/schepherd), and
every file of a release is signed and attested (see
[Verify a release](#verify-a-release)). You can also
[build from source](#build-from-source).

| Channel                       | Command                                                                                            | Needs                                |
| ----------------------------- | -------------------------------------------------------------------------------------------------- | ------------------------------------ |
| [npm](#npm)                   | `npm install --save-dev @ovineko/schepherd`                                                        | Node.js 18 or later                  |
| [PyPI](#pypi)                 | `pipx install schepherd`, `uv tool install schepherd` or `pip install schepherd`                   | Python 3.8 or later                  |
| [RubyGems](#rubygems)         | `gem install schepherd`                                                                            | Ruby 2.7 or later                    |
| [Homebrew](#homebrew)         | `brew install ovineko/tap/schepherd`                                                               | Homebrew on macOS or Linux           |
| [Scoop](#scoop)               | `scoop bucket add ovineko https://github.com/ovineko/scoop-bucket`, then `scoop install schepherd` | Scoop on Windows                     |
| [winget](#winget)             | `winget install ovineko.schepherd`                                                                 | winget on Windows                    |
| [Archives](#release-archives) | download, verify, extract                                                                          | nothing                              |
| [Go](#go)                     | `go install github.com/ovineko/schepherd/cmd/schepherd@v0.1.0`                                     | Go, the version of `go.mod` or later |

## Supported platforms

| Platform      | `GOOS/GOARCH`   | Status                                                             |
| ------------- | --------------- | ------------------------------------------------------------------ |
| Linux x64     | `linux/amd64`   | Supported                                                          |
| Linux arm64   | `linux/arm64`   | Supported (tested natively in CI, including the launcher installs) |
| macOS x64     | `darwin/amd64`  | Experimental                                                       |
| macOS arm64   | `darwin/arm64`  | Experimental                                                       |
| Windows x64   | `windows/amd64` | Experimental                                                       |
| Windows arm64 | `windows/arm64` | Experimental                                                       |

What each channel installs on a platform:

| Platform      | Archive                                   | npm package                       | PyPI wheel platform tags                                                   | Binary in the gem                               |
| ------------- | ----------------------------------------- | --------------------------------- | -------------------------------------------------------------------------- | ----------------------------------------------- |
| Linux x64     | `schepherd_<version>_linux_amd64.tar.gz`  | `@ovineko/schepherd-linux-x64`    | `manylinux2014_x86_64`, `manylinux_2_17_x86_64`, `musllinux_1_1_x86_64`    | `libexec/schepherd-linux-x64/schepherd`         |
| Linux arm64   | `schepherd_<version>_linux_arm64.tar.gz`  | `@ovineko/schepherd-linux-arm64`  | `manylinux2014_aarch64`, `manylinux_2_17_aarch64`, `musllinux_1_1_aarch64` | `libexec/schepherd-linux-arm64/schepherd`       |
| macOS x64     | `schepherd_<version>_darwin_amd64.tar.gz` | `@ovineko/schepherd-darwin-x64`   | `macosx_13_0_x86_64`                                                       | `libexec/schepherd-darwin-x64/schepherd`        |
| macOS arm64   | `schepherd_<version>_darwin_arm64.tar.gz` | `@ovineko/schepherd-darwin-arm64` | `macosx_13_0_arm64`                                                        | `libexec/schepherd-darwin-arm64/schepherd`      |
| Windows x64   | `schepherd_<version>_windows_amd64.zip`   | `@ovineko/schepherd-win32-x64`    | `win_amd64`                                                                | `libexec/schepherd-windows-x64/schepherd.exe`   |
| Windows arm64 | `schepherd_<version>_windows_arm64.zip`   | `@ovineko/schepherd-win32-arm64`  | `win_arm64`                                                                | `libexec/schepherd-windows-arm64/schepherd.exe` |

`<version>` is the release version `X.Y.Z`, for example `0.1.0`, or
`X.Y.Z-<prerelease>` for a pre-release (see
[Versioning](versioning.md#client-versions)). PyPI and RubyGems spell
pre-release versions their own way (see
[Pre-releases](versioning.md#pre-releases)).

- The Linux binaries are statically linked: the same binary, and the same
  wheel, runs on glibc and musl distributions such as Alpine.
- Every macOS artifact needs macOS 13 Ventura or later.
- On Windows, a `runner.command` that resolves to a `.bat` or `.cmd` file is
  refused (exit code 7) because Windows would run it through `cmd.exe`; see
  [Executable resolution](configuration.md#executable-resolution).
- File matching is case-sensitive on every platform (see
  [File matching](matching.md)).

**Experimental** means a release is built only after the CI test matrix
passed on that operating system (unit and integration tests, the client smoke
test and the launcher tests and smokes), but the end-to-end suite with real
registries runs on Linux x64 only and the platform has not yet seen real use.

## npm

```bash
npm install --save-dev @ovineko/schepherd
npx schepherd version
```

A plain install gets the dist-tag `latest`; pre-releases are published under
`next` (see [Pre-releases](versioning.md#pre-releases)).

[`@ovineko/schepherd`](https://www.npmjs.com/package/@ovineko/schepherd) is
a small Node.js launcher (Node.js 18 or newer, no dependencies of its own)
that lists one package per platform as an exact-version optional dependency;
npm installs only the one that matches your system. A platform package
contains only the native binary, `package.json`, `README.md`, `LICENSE` and
`THIRD_PARTY_LICENSES.txt`.

- The launcher passes arguments, standard input, standard output, standard
  error and the exit status through unchanged. On Linux and macOS it forwards
  `SIGINT`, `SIGTERM` and `SIGHUP` to the binary and, when a signal ended the
  binary, ends itself with the same signal.
- It never downloads anything, neither at install time (there is no
  `postinstall` script) nor at run time.
- With `--omit=optional` (or `--no-optional`) the platform package is not
  installed, and the launcher exits with status 1 naming the package it
  expected. Set `SCHEPHERD_BINARY` to the absolute path of a `schepherd`
  binary instead; a relative path, a missing file or a directory is refused
  with status 1.
- On a platform without a package the launcher exits with status 1, lists
  the supported platforms and suggests `SCHEPHERD_BINARY`.
- Bun can run the launcher as well.

Version `0.0.1` of `@ovineko/schepherd` and of each platform package only
reserves the package name and contains no binary. Install `0.1.0` or later.

## PyPI

```bash
pipx install schepherd
schepherd version
```

`uv tool install schepherd` and `pip install schepherd` (in a virtual
environment) install the same command, and `python -m schepherd` runs it as
well. The project is [`schepherd`](https://pypi.org/project/schepherd/) on
PyPI.

Each release has one wheel per platform,
`schepherd-<version>-py3-none-<platform tags>.whl` (tags in the
[table above](#supported-platforms)). A wheel contains exactly one native
binary (`schepherd/bin/schepherd`, `schepherd.exe` on Windows), a small
Python launcher (Python 3.8 or later, no dependencies), and `LICENSE` and
`THIRD_PARTY_LICENSES.txt` in `schepherd-<version>.dist-info/licenses/`.
Nothing is compiled or downloaded at install time. There is no source
distribution: on a platform without a wheel, pip reports that no matching
distribution was found instead of installing a launcher without a binary.

- On Linux and macOS the launcher replaces itself with the binary, which then
  receives the arguments, standard input, standard output, standard error and
  signals directly; its exit status is the command's. The signals Python
  ignores at startup (`SIGPIPE`, `SIGXFSZ`) are reset to their defaults
  first, so the binary starts as it would from a shell. On Windows, which has
  no such replacement, the launcher runs the binary, waits for it and exits
  with its exit status; Ctrl+C reaches the binary through the console.
- The launcher never downloads anything and has no override for the binary.
  On a platform without a wheel, use a [release archive](#release-archives)
  or [`go install`](#go).
- Pre-releases have PEP 440 versions such as `0.2.0rc1` (see
  [Pre-releases](versioning.md#pre-releases)).

## RubyGems

```bash
gem install schepherd
schepherd version
```

With Bundler, add the gem to the `Gemfile` and run it through Bundler:

```ruby
gem "schepherd", require: false
```

```bash
bundle exec schepherd version
```

One gem, [`schepherd`](https://rubygems.org/gems/schepherd), serves every
platform. It holds the six binaries under
`libexec/schepherd-<os>-<cpu>/` (see the [table above](#supported-platforms)),
the executable `bin/schepherd`, `lib/schepherd.rb`, `README.md`, `LICENSE`
and `THIRD_PARTY_LICENSES.txt`. It needs Ruby 2.7 or later and has no
dependencies.

- The executable picks the binary for the running platform and runs it with
  `exec`, never through a shell: the arguments, standard input, standard
  output, standard error and the exit status are those of `schepherd`.
- Set `SCHEPHERD_BINARY` to the absolute path of a `schepherd` binary to run
  that one instead, for example on a platform without a bundled binary. The
  executable exits with status 1 for a relative path or a path that is not a
  regular file, and, without the variable, on a platform without a bundled
  binary, where it lists the supported platforms.
- The gem never downloads anything, neither at install time (it has no
  extensions) nor at run time.
- Pre-releases have RubyGems versions such as `0.2.0.rc.1` (see
  [Pre-releases](versioning.md#pre-releases)).
- `gem fetch schepherd -v <gem version>` downloads the gem,
  `schepherd-<gem version>.gem`; after verifying it (see
  [Verify a release](#verify-a-release)), install it without the registry
  with `gem install --local ./schepherd-<gem version>.gem`.

## Homebrew

```bash
brew install ovineko/tap/schepherd
schepherd version
```

The cask `schepherd` lives in the tap
[`ovineko/homebrew-tap`](https://github.com/ovineko/homebrew-tap), which
`brew install ovineko/tap/schepherd` adds by itself. It installs the binary of
the [release archive](#release-archives) for macOS or Linux, x64 or arm64,
checked against the SHA-256 the cask records; `brew upgrade` follows new
releases. The binaries are not notarized by Apple, so on macOS the cask
removes the quarantine attribute from the installed binary.

## Scoop

```powershell
scoop bucket add ovineko https://github.com/ovineko/scoop-bucket
scoop install schepherd
schepherd version
```

The manifest `schepherd` in the bucket
[`ovineko/scoop-bucket`](https://github.com/ovineko/scoop-bucket) installs the
Windows x64 or arm64 [release archive](#release-archives), checked against its
SHA-256, and puts `schepherd.exe` on the `PATH`; `scoop update schepherd`
follows new releases.

## winget

```powershell
winget install ovineko.schepherd
schepherd version
```

The package `ovineko.schepherd` installs the Windows x64 or arm64
[release archive](#release-archives) as a portable command, checked against
its SHA-256. Its manifests are submitted to
[microsoft/winget-pkgs](https://github.com/microsoft/winget-pkgs) with every
release, and winget offers a version only after Microsoft's review has merged
that pull request, which can take a few days.

Homebrew, Scoop and winget receive releases only, never pre-releases.

## Release archives

Each client release (tag `vX.Y.Z`, marked latest on GitHub) has one archive
per platform, an SPDX software bill of materials of every archive
(`<archive>.sbom.json`), the checksum file
`schepherd_<version>_checksums.txt` and its signature
`schepherd_<version>_checksums.txt.sigstore.json`. Every archive contains
`schepherd` (or `schepherd.exe`), `LICENSE`, `README.md`,
`THIRD_PARTY_NOTICES.md` and `THIRD_PARTY_LICENSES.txt`. The releases titled
`Schemas <revision>` (tags `catalog-*`) are schema catalog revisions and carry
no binaries.

```bash
version=0.1.0
tar -xzf "schepherd_${version}_linux_amd64.tar.gz" schepherd
install -m 0755 schepherd "$HOME/.local/bin/schepherd"   # any directory on PATH
```

### Verify the download

Check the archive against the checksum file of the same release before you
extract it:

```bash
# Linux (GNU coreutils)
sha256sum --check --ignore-missing "schepherd_${version}_checksums.txt"

# macOS
grep " schepherd_${version}_darwin_arm64.tar.gz\$" "schepherd_${version}_checksums.txt" | shasum -a 256 --check
```

```powershell
# Windows (PowerShell): the two values must be equal
(Get-FileHash -Algorithm SHA256 .\schepherd_0.1.0_windows_amd64.zip).Hash.ToLower()
Select-String -Path .\schepherd_0.1.0_checksums.txt -Pattern 'windows_amd64\.zip'
```

A checksum proves that the archive equals what the checksum file lists. It is
not a signature: it does not prove who published the release. The next
section proves that.

## Verify a release

Every release is built and published by the workflow
[`release.yml`](versioning.md#release-workflow) from the tagged commit, whose
jobs sign and attest what they publish with keyless
[Sigstore](https://www.sigstore.dev/) certificates that name this workflow
and the tag. The signatures are recorded in Sigstore's public transparency
log.

**Checksum file, cosign.** The checksum file comes with a Sigstore bundle
signed by the release workflow at the tag. Verify it with
[cosign](https://docs.sigstore.dev/cosign/system_config/installation/) 3 or
later, then check your download against the verified checksum file:

```bash
version=0.1.0
gh release download "v${version}" --repo ovineko/schepherd --pattern "schepherd_${version}_checksums.txt*"
cosign verify-blob --bundle "schepherd_${version}_checksums.txt.sigstore.json" \
  --certificate-identity "https://github.com/ovineko/schepherd/.github/workflows/release.yml@refs/tags/v${version}" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  "schepherd_${version}_checksums.txt"
sha256sum --check --ignore-missing "schepherd_${version}_checksums.txt"
```

**Any release file or package, GitHub artifact attestations.** Every archive,
SBOM, the checksum file, every npm tarball (`npm pack @ovineko/schepherd@<version>`
downloads it), wheel and the gem has a
[build provenance attestation](https://docs.github.com/en/actions/concepts/security/artifact-attestations)
made by the release workflow. With the [GitHub CLI](https://cli.github.com/):

```bash
gh attestation verify "schepherd_${version}_linux_amd64.tar.gz" --repo ovineko/schepherd \
  --signer-workflow ovineko/schepherd/.github/workflows/release.yml --source-ref "refs/tags/v${version}"
```

Releases are immutable: once published, their assets and tag cannot change,
and GitHub attests the release itself. `gh release verify "v${version}" --repo ovineko/schepherd`
checks that attestation, and
`gh release verify-asset "v${version}" <file> --repo ovineko/schepherd` checks
that a downloaded file is an asset of that release.

**npm.** npm records the build provenance of every package the workflow
publishes and shows it on the package page. In a project that installed
`@ovineko/schepherd`, `npm audit signatures` verifies the registry signatures
and provenance attestations of the installed packages.

**PyPI.** Every wheel is uploaded with a
[PEP 740](https://peps.python.org/pep-0740/) attestation of the release
workflow, shown as its provenance on PyPI. Verify one with
[`pypi-attestations`](https://pypi.org/project/pypi-attestations/):

```bash
uvx pypi-attestations verify pypi --repository https://github.com/ovineko/schepherd \
  "pypi:schepherd-${version}-py3-none-macosx_13_0_arm64.whl"
```

**RubyGems.** The gem is pushed with a Sigstore attestation that
rubygems.org checks against the gem's trusted publisher and serves through its
API:

```bash
gem fetch schepherd -v "$version"
curl -fsS "https://rubygems.org/api/v1/attestations/schepherd-${version}.json" | jq '.[0]' > "schepherd-${version}.gem.sigstore.json"
cosign verify-blob --bundle "schepherd-${version}.gem.sigstore.json" \
  --certificate-identity "https://github.com/ovineko/schepherd/.github/workflows/release.yml@refs/tags/v${version}" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  "schepherd-${version}.gem"
```

`gem fetch` and the API use the gem version, which differs from the release
version for a pre-release (see [Pre-releases](versioning.md#pre-releases)).

**Homebrew, Scoop and winget** check the release archive they install
against the SHA-256 their manifest records, taken from the checksum file.

**Schema catalog.** The catalog published to
`ghcr.io/ovineko/schepherd-schemas` has its own attestation; see
[Verifying a published catalog](security.md#verifying-a-published-catalog).

## Go

```bash
go install github.com/ovineko/schepherd/cmd/schepherd@v0.1.0
```

The Go module version of a release is its tag `vX.Y.Z` (see
[Go module tags](versioning.md#go-module-tags)); `@latest` resolves the
newest release, but pinning an exact tag is recommended. A binary built this
way reports the tag's version in `schepherd version` (see
[Binary version](versioning.md#binary-version)).

## Build from source

With the Go version from `go.mod`:

```bash
go build -trimpath -o schepherd ./cmd/schepherd
```

Such a build reports `dev`, unless it is made in a clean checkout of a
release tag, where Go records the tag as the module version.

## Third-party licenses

`THIRD_PARTY_LICENSES.txt`, in every archive, npm platform package, wheel
and the gem, holds the license, notice and patent texts of the Go standard
library and of every module linked into the binary for any of the six
targets.
`THIRD_PARTY_NOTICES.md` summarizes them. Schemas delivered through Schepherd
keep their own licenses, recorded in each catalog entry and artifact.

## Cache location

The client caches verified catalogs and schemas under the OS user cache
directory plus `/schepherd`; `--cache-dir` or `SCHEPHERD_CACHE_DIR` choose
another directory. Deleting the cache is always safe: it is rebuilt on the
next online run, and copies written by `schepherd export` do not depend on it.
