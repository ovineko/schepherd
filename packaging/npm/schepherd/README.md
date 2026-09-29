# @ovineko/schepherd

Schepherd delivers a chosen JSON Schema from a pinned OCI catalog to a
verified local file, and can hand that file to the validator you already use.

This package contains only a small Node.js launcher for the native `schepherd`
binary. npm installs the binary for your platform through one of the optional
dependencies `@ovineko/schepherd-<os>-<cpu>`. The packages contain no schemas
and no catalogs: those are fetched from the OCI registry you configure and
verified by digest.

```sh
npm install --save-dev @ovineko/schepherd
npx schepherd --help
```

## Platforms

| Platform           | Package                          | Status       |
| ------------------ | -------------------------------- | ------------ |
| Linux x64          | `@ovineko/schepherd-linux-x64`   | Supported    |
| Linux arm64        | `@ovineko/schepherd-linux-arm64` | Supported    |
| macOS x64, arm64   | `@ovineko/schepherd-darwin-*`    | Experimental |
| Windows x64, arm64 | `@ovineko/schepherd-win32-*`     | Experimental |

Every package is cross-compiled from the same commit, and a release is built
only after the unit, integration and smoke tests (npm launcher included)
passed on Linux, macOS and Windows. The end-to-end suite with real registries
runs on Linux x64 only, so the macOS and Windows packages stay experimental
until they have seen use beyond these tests; please report any problem you
find.

## Notes

- Optional dependencies must be installed. If you install with
  `--omit=optional`, set `SCHEPHERD_BINARY` to the absolute path of a
  `schepherd` binary instead.
- The launcher never downloads anything.
- Each platform package also contains `THIRD_PARTY_LICENSES.txt` with the
  license texts of the third-party code linked into the binary.

Installation options, checksum verification and the platform status:
<https://schepherd.ovineko.com/installation/>

Documentation and configuration reference: <https://schepherd.ovineko.com>

Source code, issues and release notes: <https://github.com/ovineko/schepherd>

License: MIT
