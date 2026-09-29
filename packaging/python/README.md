# schepherd

Schepherd delivers a chosen JSON Schema from a pinned OCI catalog to a
verified local file, and can hand that file to the validator you already use.

This package installs the native `schepherd` binary for your platform with a
small Python launcher, so pip, pipx and uv can install the command. Each wheel
contains exactly one binary; nothing is compiled or downloaded during
installation. The package contains no schemas and no catalogs: those are
fetched from the OCI registry you configure and verified by digest.

```sh
pipx install schepherd
schepherd --help
```

`uv tool install schepherd` and `pip install schepherd` work the same way, and
`python -m schepherd` runs the same command.

## Platforms

| Platform                            | Wheel platform tags                                | Status       |
| ----------------------------------- | -------------------------------------------------- | ------------ |
| Linux x86_64, aarch64               | `manylinux_2_17`, `manylinux2014`, `musllinux_1_1` | Supported    |
| macOS 13 or later, x86_64 and arm64 | `macosx_13_0`                                      | Experimental |
| Windows x64, ARM64                  | `win_amd64`, `win_arm64`                           | Experimental |

The Linux binaries are statically linked, so the same wheel installs on glibc
and musl distributions such as Alpine. There is no source distribution: on any
other platform pip reports that no matching distribution exists instead of
installing a package without a binary. The launcher needs Python 3.8 or later.

## Notes

- On Linux and macOS the launcher replaces itself with the binary, which then
  receives the arguments, standard streams and signals directly. On Windows it
  runs the binary and exits with its exit status.
- The launcher never downloads anything.
- The wheel's `.dist-info/licenses/` directory contains `LICENSE` and
  `THIRD_PARTY_LICENSES.txt` with the license texts of the third-party code
  linked into the binary.
- Pre-releases (`X.Y.ZaN`, `X.Y.ZbN`, `X.Y.ZrcN`) are installed only with
  `--pre` or an exact version, for example `pipx install schepherd==0.2.0rc1`.

Installation options and the platform status:
<https://schepherd.ovineko.com/installation/>

Documentation and configuration reference: <https://schepherd.ovineko.com>

Source code, issues and release notes: <https://github.com/ovineko/schepherd>

License: MIT
