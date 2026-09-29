# frozen_string_literal: true

# The gem of the schepherd binary: the executable, the library that selects
# the binary of the running platform, and the binaries of every release
# target under libexec/. It is built only by
# `go run ./tools/release packages build`, which stages this directory with
# the binaries, LICENSE and THIRD_PARTY_LICENSES.txt and sets
# SCHEPHERD_GEM_VERSION and SOURCE_DATE_EPOCH.
version = ENV.fetch("SCHEPHERD_GEM_VERSION")

Gem::Specification.new do |spec|
  spec.name = "schepherd"
  spec.version = version
  spec.authors = ["ovineko"]
  spec.summary = "Delivers pinned JSON Schemas from an OCI catalog to verified local files"
  spec.description = "Schepherd resolves a JSON Schema from a pinned OCI catalog, verifies it by digest and " \
                     "hands the local file to the validator you already use. The gem ships the native schepherd " \
                     "binary for Linux, macOS and Windows on x64 and arm64."
  spec.homepage = "https://schepherd.ovineko.com"
  spec.license = "MIT"
  spec.required_ruby_version = ">= 2.7"
  spec.metadata = {
    # A snapshot names a host that does not exist (RFC 2606), so `gem push`
    # refuses to send it to rubygems.org.
    "allowed_push_host" => version.include?(".snapshot.") ? "https://snapshot.invalid" : "https://rubygems.org",
    "bug_tracker_uri" => "https://github.com/ovineko/schepherd/issues",
    "changelog_uri" => "https://github.com/ovineko/schepherd/releases",
    "documentation_uri" => "https://schepherd.ovineko.com",
    "rubygems_mfa_required" => "true",
    "source_code_uri" => "https://github.com/ovineko/schepherd",
  }
  spec.bindir = "bin"
  spec.executables = ["schepherd"]
  spec.require_paths = ["lib"]
  spec.files = (%w[LICENSE README.md THIRD_PARTY_LICENSES.txt bin/schepherd lib/schepherd.rb] +
                Dir.glob("libexec/schepherd-*/schepherd{,.exe}")).sort
end
