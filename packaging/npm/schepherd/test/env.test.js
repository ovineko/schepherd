"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const { test } = require("node:test");

const { binaryOverride, EnvError } = require("../lib/env.js");
const { binaryName, isSupported, packageName, TARGETS } = require("../lib/platform.js");
const { tempDir } = require("./helpers.js");

test("SCHEPHERD_BINARY is optional", () => {
  assert.equal(binaryOverride({}), undefined);
  assert.equal(binaryOverride({ SCHEPHERD_BINARY: "" }), undefined);
});

test("SCHEPHERD_BINARY must be an absolute path to a regular file", (t) => {
  const dir = tempDir(t);
  const file = path.join(dir, "schepherd");
  fs.writeFileSync(file, "", { mode: 0o755 });

  assert.equal(binaryOverride({ SCHEPHERD_BINARY: file }), file);

  const cases = [
    ["bin/schepherd", /must be an absolute path/],
    [path.join(dir, "missing"), /cannot be read \(ENOENT\)/],
    [dir, /is not a regular file/],
  ];

  for (const [value, message] of cases) {
    assert.throws(
      () => binaryOverride({ SCHEPHERD_BINARY: value }),
      (error) => error instanceof EnvError && message.test(error.message),
    );
  }
});

test("platform table covers the release targets", () => {
  assert.deepEqual(
    TARGETS.map((target) => `${target.platform}-${target.arch}`),
    ["darwin-arm64", "darwin-x64", "linux-arm64", "linux-x64", "win32-arm64", "win32-x64"],
  );
  assert.equal(packageName("linux", "x64"), "@ovineko/schepherd-linux-x64");
  assert.equal(binaryName("win32"), "schepherd.exe");
  assert.equal(binaryName("darwin"), "schepherd");
  assert.equal(isSupported("linux", "ia32"), false);
  assert.throws(() => {
    TARGETS.push({ arch: "x", platform: "y" });
  }, TypeError);
});
