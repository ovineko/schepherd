/// <reference path=".datamitsu/datamitsu.config.d.ts" />

const golangciVersion = "2.15.0";
const golangciHashes = {
  darwin: {
    amd64: "sha256:767b899a28285e88ded2823d5dcc4bc5ef681173eb2d80f029a7b17ee4ece3b8",
    arm64: "sha256:d1e409481a8ca846c4bc97e781eb5cb1bd5c23353a0b3977a1cbda1b1b6447d5",
  },
  freebsd: {
    amd64: "sha256:4af60f7a5f057a0996de9642e9206db4e06666c5e191e53af7c360c26e60f1ef",
    arm64: "sha256:7780ed9c5ec5b1f184b3d7ba700695312afd773b7a9a946369ffea2169030a12",
  },
  linux: {
    amd64: "sha256:9b7fe6a6bf6334b98decf8d86b11415ea7889afdabdd1959fdce23af2a9ebcea",
    arm64: "sha256:b4d8f17068b35ee6060cc2f69a16fb48740d706fbba81661c318649c114a6998",
  },
  windows: {
    amd64: "sha256:cd7ea6063c95de245e17e5246e42bdc14bb221792c429dbd239ab3a6ade6ca6a",
    arm64: "sha256:4fb8988abf186158e30af1b8a0fb3671cf2cb02bc91d7e6ce6996f4c1e922e9a",
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
      uv: {
        binary: { binaries: input.runtimes.uv.managed.binaries },
      },
    },
    ejectConfigs: [...new Set([...(input.ejectConfigs ?? []), "gitleaks"])],
    tools: {
      ...input.tools,
      bearer: {
        ...input.tools.bearer,
        operations: {
          lint: {
            ...input.tools.bearer.operations.lint,
            args: [
              "scan",
              "--exit-code",
              "1",
              "--skip-path",
              ".claude/worktrees/**,parsers/target/**,tools/check-jsonschema/.venv/**",
              "{target}",
            ],
          },
        },
      },
      eslint: {
        ...input.tools.eslint,
        operations: {
          fix: {
            ...input.tools.eslint.operations.fix,
            args: [
              "--quiet",
              "--format=json",
              "--fix",
              "--fix-type",
              "problem,suggestion,layout",
              "-c",
              "{root}/eslint.config.mjs",
              "{files}",
            ],
          },
          lint: {
            ...input.tools.eslint.operations.lint,
            args: ["--quiet", "--format=json", "-c", "{root}/eslint.config.mjs", "{files}"],
          },
        },
      },
      grype: {
        ...input.tools.grype,
        operations: {
          ...input.tools.grype.operations,
          lint: {
            ...input.tools.grype.operations.lint,
            args: [
              ...input.tools.grype.operations.lint.args,
              "--exclude",
              "./.claude/worktrees/**",
              "--exclude",
              "./.tools/**",
              "--exclude",
              "./parsers/target/**",
              "--exclude",
              "./tools/check-jsonschema/.venv/**",
            ],
          },
        },
      },
      oxlint: {
        ...input.tools.oxlint,
        operations: {
          fix: {
            ...input.tools.oxlint.operations.fix,
            args: [
              "--disable-nested-config",
              "-c",
              "{root}/oxlint.config.mts",
              "--format=default",
              "--fix",
              "{files}",
            ],
          },
          lint: {
            ...input.tools.oxlint.operations.lint,
            args: [
              "--disable-nested-config",
              "-c",
              "{root}/oxlint.config.mts",
              "--format=default",
              "{files}",
            ],
          },
        },
      },
    },
  };
}
globalThis.getConfig = getConfig;
