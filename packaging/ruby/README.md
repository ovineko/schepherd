# schepherd

Schepherd delivers a chosen JSON Schema from a pinned OCI catalog to a
verified local file, and can hand that file to the validator you already use.

This gem contains the native `schepherd` binary of every supported platform
and a small Ruby executable that runs the one built for your system. It
contains no schemas and no catalogs: those are fetched from the OCI registry
you configure and verified by digest.

```sh
gem install schepherd
schepherd --help
```

With Bundler:

```ruby
gem "schepherd", require: false
```

```sh
bundle exec schepherd --help
```

## Platforms

| Platform           | Binary in the gem               | Status       |
| ------------------ | ------------------------------- | ------------ |
| Linux x64          | `libexec/schepherd-linux-x64`   | Supported    |
| Linux arm64        | `libexec/schepherd-linux-arm64` | Supported    |
| macOS x64, arm64   | `libexec/schepherd-darwin-*`    | Experimental |
| Windows x64, arm64 | `libexec/schepherd-windows-*`   | Experimental |

The binaries are the same files as in the GitHub release archives of the
version, cross-compiled from one commit. The Linux binaries are static, so
they run on glibc and musl systems alike; the macOS binaries need macOS 13
or later. The gem needs Ruby 2.7 or later and has no dependencies.

## Notes

- The executable replaces itself with the binary: arguments, standard input
  and output, and the exit status are those of `schepherd` itself.
- On a platform without a prebuilt binary, set `SCHEPHERD_BINARY` to the
  absolute path of a `schepherd` binary built for it.
- The gem never downloads anything.
- `THIRD_PARTY_LICENSES.txt` holds the license texts of the third-party code
  linked into the binaries.
- Pre-releases (`X.Y.Z.alpha.N`, `X.Y.Z.beta.N`, `X.Y.Z.rc.N`) are installed
  only with `--pre` or an exact version, for example
  `gem install schepherd -v 0.2.0.rc.1`.

Installation options and the platform status:
<https://schepherd.ovineko.com/installation/>

Documentation and configuration reference: <https://schepherd.ovineko.com>

Source code, issues and release notes: <https://github.com/ovineko/schepherd>

License: MIT
