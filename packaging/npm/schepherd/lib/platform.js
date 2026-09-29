"use strict";

// The same table exists in tools/release/internal/wrappers/targets.go, which
// generates the platform packages; a Go test keeps the two identical.
const ROOT_PACKAGE = "@ovineko/schepherd";

const TARGETS = Object.freeze([
  Object.freeze({ arch: "arm64", platform: "darwin" }),
  Object.freeze({ arch: "x64", platform: "darwin" }),
  Object.freeze({ arch: "arm64", platform: "linux" }),
  Object.freeze({ arch: "x64", platform: "linux" }),
  Object.freeze({ arch: "arm64", platform: "win32" }),
  Object.freeze({ arch: "x64", platform: "win32" }),
]);

function binaryName(platform) {
  return platform === "win32" ? "schepherd.exe" : "schepherd";
}

function isSupported(platform, arch) {
  return TARGETS.some((target) => target.platform === platform && target.arch === arch);
}

function packageName(platform, arch) {
  return `${ROOT_PACKAGE}-${platform}-${arch}`;
}

module.exports = { binaryName, isSupported, packageName, ROOT_PACKAGE, TARGETS };
