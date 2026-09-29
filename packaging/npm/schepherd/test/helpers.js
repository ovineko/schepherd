"use strict";

const { spawnSync } = require("node:child_process");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { after } = require("node:test");

const LAUNCHER = path.join(__dirname, "..", "bin", "schepherd.js");

// A stand-in for the native binary. FAKE_MODE selects its behaviour so the
// arguments stay entirely under the control of each test.
const FAKE_BINARY = `
"use strict";
const mode = process.env.FAKE_MODE;
if (mode === "args") {
  process.stdout.write(JSON.stringify(process.argv.slice(2)));
} else if (mode === "exit") {
  process.exit(Number(process.env.FAKE_CODE));
} else if (mode === "self-signal") {
  process.kill(process.pid, "SIGTERM");
  setInterval(() => {}, 1000);
} else if (mode === "stdin") {
  const hash = require("node:crypto").createHash("sha256");
  let size = 0;
  process.stdin.on("data", (chunk) => {
    hash.update(chunk);
    size += chunk.length;
  });
  process.stdin.on("end", () => {
    process.stdout.write(JSON.stringify({ sha256: hash.digest("hex"), size }));
  });
} else if (mode === "trap") {
  process.on("SIGTERM", () => {
    process.stdout.write("got SIGTERM\\n");
    process.exit(42);
  });
  process.stdout.write("ready\\n");
  setInterval(() => {}, 1000);
}
`;

const WINDOWS = process.platform === "win32";

// Set on the tests that send POSIX signals, which Windows does not have.
const POSIX_SIGNALS_ONLY = WINDOWS && "Windows has no POSIX signals";

let windowsFake;

after(() => {
  if (windowsFake !== undefined) {
    fs.rmSync(path.dirname(windowsFake), { force: true, recursive: true });
  }
});

// Windows programs expect SystemRoot even in an otherwise minimal environment.
function childEnv(vars) {
  return WINDOWS ? { SystemRoot: process.env.SystemRoot, ...vars } : vars;
}

function fakeBinary(t) {
  if (WINDOWS) {
    return windowsFakeBinary();
  }

  const file = path.join(tempDir(t), "schepherd");
  fs.writeFileSync(file, `#!${process.execPath}\n${FAKE_BINARY}`, { mode: 0o755 });
  return file;
}

function tempDir(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "schepherd-launcher-"));
  t.after(() => fs.rmSync(dir, { force: true, recursive: true }));
  return dir;
}

// Windows cannot start a script through its #! line, so there the stand-in is
// a single executable application: a copy of this Node.js runtime whose entry
// point is FAKE_BINARY and which leaves every argument to it. The copy is as
// large as the runtime, so each test file builds it once.
function windowsFakeBinary() {
  if (windowsFake !== undefined) {
    return windowsFake;
  }

  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "schepherd-launcher-"));
  const config = path.join(dir, "sea.json");
  const binary = path.join(dir, "schepherd.exe");

  fs.writeFileSync(path.join(dir, "fake.js"), FAKE_BINARY);
  fs.writeFileSync(
    config,
    JSON.stringify({
      disableExperimentalSEAWarning: true,
      main: path.join(dir, "fake.js"),
      output: binary,
    }),
  );

  const result = spawnSync(process.execPath, ["--build-sea", config], {
    encoding: "utf8",
    timeout: 300_000,
  });
  if (result.status !== 0) {
    fs.rmSync(dir, { force: true, recursive: true });
    throw new Error(`building the stand-in binary failed (${result.status}): ${result.stderr}`);
  }

  windowsFake = binary;
  return binary;
}

module.exports = { childEnv, fakeBinary, LAUNCHER, POSIX_SIGNALS_ONLY, tempDir };
