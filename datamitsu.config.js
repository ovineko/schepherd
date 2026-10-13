/// <reference path=".datamitsu/datamitsu.config.d.ts" />

const golangciVersion = "2.14.0";
const golangciHashes = {
  darwin: {
    amd64: "sha256:a5667c1c3536be1740133213e1e822bfb8f0d98ea12903174d6d5f635e4ed68d",
    arm64: "sha256:5ef5f36a7147e91dc58ef9ef4d11bb7bad5ead0c76eb6c01327a73c641d1dcc3",
  },
  freebsd: {
    amd64: "sha256:5224b2e8637cb9036025d1b028c4b509c7fc7857708840e31b5a79743762d9bf",
    arm64: "sha256:f6810108716c7c602943b988dfa1af469d6e97e5fe6e0595784c45dff0c245dc",
  },
  linux: {
    amd64: "sha256:ab90aeb7b066f92a33415b638a50fe5344bbb75a0d32ad30cc248d88f81032ab",
    arm64: "sha256:ee7ec5f3453d15ddf106fae5a4d6c71737712348a979d1fe9cd52ec7ea299bae",
  },
  windows: {
    amd64: "sha256:b15f903d1649283a2ab6e8d2f0c317eaf1f9f6b3763950e7e8b7e72a43dd724f",
    arm64: "sha256:6399496aef883e5d2f77ea93e6e54fdd05bc2695f64071d8de0b0579cad92274",
  },
};

function getMinVersion() {
  return "0.5.1";
}
globalThis.getMinVersion = getMinVersion;

function getConfig(input) {
  const binaries = {};
  for (const [os, architectures] of Object.entries(golangciHashes)) {
    binaries[os] = {};
    for (const [arch, hash] of Object.entries(architectures)) {
      const contentType = os === "windows" ? "zip" : "tar.gz";
      binaries[os][arch] = {
        [os === "linux" ? "glibc" : "unknown"]: {
          binaryPath: `golangci-lint-${golangciVersion}/golangci-lint${os === "windows" ? ".exe" : ""}`,
          contentType,
          hash,
          url: `https://github.com/golangci/golangci-lint/releases/download/v${golangciVersion}/golangci-lint-${golangciVersion}-${os}-${arch}.${contentType}`,
        },
      };
    }
  }
  return {
    ...input,
    apps: {
      ...input.apps,
      "golangci-lint": {
        ...input.apps["golangci-lint"],
        binary: { binaries, version: `v${golangciVersion}` },
      },
    },
  };
}
globalThis.getConfig = getConfig;
