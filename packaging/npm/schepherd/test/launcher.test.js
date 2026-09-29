"use strict";

const assert = require("node:assert/strict");
const { spawn, spawnSync } = require("node:child_process");
const { createHash } = require("node:crypto");
const fs = require("node:fs");
const path = require("node:path");
const { test } = require("node:test");

const { LauncherError, resolveBinary, run } = require("../lib/launcher.js");
const { childEnv, fakeBinary, LAUNCHER, POSIX_SIGNALS_ONLY, tempDir } = require("./helpers.js");

function launch(t, mode, args, extraEnv = {}, input) {
  return spawnSync(process.execPath, [LAUNCHER, ...args], {
    encoding: "utf8",
    env: childEnv({ FAKE_MODE: mode, SCHEPHERD_BINARY: fakeBinary(t), ...extraEnv }),
    input,
    timeout: 30_000,
  });
}

const noOverride = () => undefined;

test("passes every argument through unchanged, including --", (t) => {
  const args = ["validate", "--", "--not-a-flag", "two words", "", "ünïcode", "-x"];
  const result = launch(t, "args", args);

  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(JSON.parse(result.stdout), args);
});

test("passes stdin through to the binary byte for byte until end of input", (t) => {
  // Larger than any pipe buffer, with every byte value, so that a launcher
  // that read, decoded or truncated stdin would change the digest.
  const input = Buffer.alloc(3 * 1024 * 1024 + 7);
  for (let i = 0; i < input.length; i++) {
    input[i] = (i * 131 + (i >> 8)) & 0xff;
  }

  for (const payload of [input, Buffer.alloc(0)]) {
    const result = launch(t, "stdin", [], {}, payload);

    assert.equal(result.status, 0, result.stderr);
    assert.deepEqual(JSON.parse(result.stdout), {
      sha256: createHash("sha256").update(payload).digest("hex"),
      size: payload.length,
    });
  }
});

test("exits with the binary's exit code", (t) => {
  for (const code of [0, 3, 7, 124]) {
    const result = launch(t, "exit", [], { FAKE_CODE: String(code) });
    assert.equal(result.status, code, result.stderr);
  }
});

test("re-raises the signal that terminated the binary", { skip: POSIX_SIGNALS_ONLY }, (t) => {
  const result = launch(t, "self-signal", []);

  assert.equal(result.status, null);
  assert.equal(result.signal, "SIGTERM");
});

test(
  "forwards SIGTERM to the binary and mirrors its exit",
  { skip: POSIX_SIGNALS_ONLY },
  async (t) => {
    const child = spawn(process.execPath, [LAUNCHER], {
      env: childEnv({ FAKE_MODE: "trap", SCHEPHERD_BINARY: fakeBinary(t) }),
      stdio: ["ignore", "pipe", "pipe"],
    });

    let stdout = "";
    const exited = new Promise((resolve) =>
      child.on("exit", (code, signal) => resolve({ code, signal })),
    );

    await new Promise((resolve, reject) => {
      child.stdout.on("data", (chunk) => {
        stdout += chunk;
        if (stdout.includes("ready")) {
          resolve();
        }
      });
      child.on("error", reject);
    });

    child.kill("SIGTERM");

    const { code, signal } = await exited;
    assert.equal(signal, null);
    assert.equal(code, 42);
    assert.match(stdout, /got SIGTERM/);
  },
);

function emitter() {
  const listeners = new Map();
  const listenersOf = (event) => listeners.get(event) || [];
  return {
    emit(event, ...args) {
      const current = listenersOf(event);
      for (const listener of current) {
        listener(...args);
      }
      return current.length > 0;
    },
    listenerCount: (event) => listenersOf(event).length,
    on(event, listener) {
      listeners.set(event, [...listenersOf(event), listener]);
      return this;
    },
    removeListener(event, listener) {
      listeners.set(
        event,
        listenersOf(event).filter((candidate) => candidate !== listener),
      );
      return this;
    },
  };
}

function fakeChild() {
  const child = emitter();
  child.exitCode = null;
  child.signalCode = null;
  child.kills = [];
  child.kill = (signal) => child.kills.push(signal);
  return child;
}

function fakeProcess() {
  const proc = emitter();
  proc.pid = 4242;
  proc.exits = [];
  proc.kills = [];
  proc.exit = (code) => proc.exits.push(code);
  proc.kill = (pid, signal) => proc.kills.push([pid, signal]);
  proc.stderr = { write: () => true };
  return proc;
}

test("on Windows leaves console signals to the binary and mirrors its exit code", () => {
  const proc = fakeProcess();
  const child = fakeChild();
  run("schepherd.exe", [], { platform: "win32", proc, spawnChild: () => child });

  for (const signal of ["SIGINT", "SIGBREAK", "SIGHUP"]) {
    assert.ok(proc.emit(signal), `the launcher must survive ${signal}`);
  }
  assert.deepEqual(child.kills, []);

  child.emit("exit", 130, null);
  assert.deepEqual(proc.exits, [130]);
  assert.deepEqual(proc.kills, []);
  assert.equal(proc.listenerCount("SIGINT"), 0);
});

test("on Windows exits with a status instead of re-raising a signal", () => {
  const proc = fakeProcess();
  const child = fakeChild();
  run("schepherd.exe", [], { platform: "win32", proc, spawnChild: () => child });

  child.emit("exit", null, "SIGTERM");
  assert.deepEqual(proc.kills, []);
  assert.equal(proc.exits.length, 1);
  assert.ok(proc.exits[0] > 128, `exit status ${proc.exits[0]}`);
});

test("on POSIX forwards signals and re-raises the one that ended the binary", (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const proc = fakeProcess();
  const child = fakeChild();
  run("schepherd", [], { platform: "linux", proc, spawnChild: () => child });

  proc.emit("SIGINT");
  assert.deepEqual(child.kills, ["SIGINT"]);

  child.emit("exit", null, "SIGINT");
  assert.deepEqual(proc.kills, [[4242, "SIGINT"]]);
  assert.deepEqual(proc.exits, []);

  t.mock.timers.tick(1000);
  assert.deepEqual(proc.exits, [130]);
});

test("reports an invalid SCHEPHERD_BINARY and exits 1", () => {
  const result = spawnSync(process.execPath, [LAUNCHER], {
    encoding: "utf8",
    env: childEnv({ SCHEPHERD_BINARY: "relative/schepherd" }),
  });

  assert.equal(result.status, 1);
  assert.match(result.stderr, /SCHEPHERD_BINARY must be an absolute path/);
});

test("names the expected package on an unsupported platform", () => {
  assert.throws(
    () => resolveBinary({ arch: "ppc64", override: noOverride, platform: "aix" }),
    (error) => {
      assert.ok(error instanceof LauncherError);
      assert.match(error.message, /@ovineko\/schepherd-aix-ppc64/);
      assert.match(error.message, /SCHEPHERD_BINARY/);
      assert.match(error.message, /linux-x64/);
      return true;
    },
  );
});

test("explains a missing optional platform package", () => {
  const resolvePackage = () => {
    const error = new Error("Cannot find module");
    error.code = "MODULE_NOT_FOUND";
    throw error;
  };

  assert.throws(
    () => resolveBinary({ arch: "x64", override: noOverride, platform: "linux", resolvePackage }),
    (error) => {
      assert.ok(error instanceof LauncherError);
      assert.match(error.message, /@ovineko\/schepherd-linux-x64 is not installed/);
      assert.match(error.message, /--omit=optional/);
      assert.match(error.message, /SCHEPHERD_BINARY/);
      return true;
    },
  );
});

test("resolves the binary inside the platform package", (t) => {
  for (const [platform, binaryName] of [
    ["linux", "schepherd"],
    ["win32", "schepherd.exe"],
  ]) {
    const packageDir = tempDir(t);
    fs.mkdirSync(path.join(packageDir, "bin"));
    fs.writeFileSync(path.join(packageDir, "package.json"), "{}");

    const requests = [];
    const resolvePackage = (request) => {
      requests.push(request);
      return path.join(packageDir, "package.json");
    };
    const options = { arch: "arm64", override: noOverride, platform, resolvePackage };

    assert.throws(() => resolveBinary(options), /binary .* is missing/);

    fs.writeFileSync(path.join(packageDir, "bin", binaryName), "");
    assert.equal(resolveBinary(options), path.join(packageDir, "bin", binaryName));
    assert.deepEqual(requests, [
      `@ovineko/schepherd-${platform}-arm64/package.json`,
      `@ovineko/schepherd-${platform}-arm64/package.json`,
    ]);
  }
});

test("SCHEPHERD_BINARY takes precedence over the platform package", () => {
  const resolvePackage = () => assert.fail("the platform package must not be resolved");

  assert.equal(
    resolveBinary({
      arch: "ppc64",
      override: () => "/opt/schepherd",
      platform: "aix",
      resolvePackage,
    }),
    "/opt/schepherd",
  );
});
