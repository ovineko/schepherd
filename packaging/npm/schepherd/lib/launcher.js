"use strict";

const { spawn } = require("node:child_process");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");

const { BINARY_OVERRIDE, binaryOverride } = require("./env.js");
const { binaryName, isSupported, packageName, ROOT_PACKAGE, TARGETS } = require("./platform.js");

const FORWARDED_SIGNALS = Object.freeze(["SIGINT", "SIGTERM", "SIGHUP"]);

// On Windows the console delivers Ctrl-C, Ctrl-Break and window close to
// every process attached to it, the binary included, and child.kill() would
// terminate the binary forcefully instead of letting it shut down. The
// launcher only listens so that Node does not exit before the binary does.
const WINDOWS_CONSOLE_SIGNALS = Object.freeze(["SIGINT", "SIGBREAK", "SIGHUP"]);

// How long to wait for a re-raised signal to end the launcher before falling
// back to the conventional 128+n exit status, for signals Node ignores such
// as SIGPIPE.
const RERAISE_GRACE_MS = 500;

class LauncherError extends Error {
  constructor(message) {
    super(message);
    this.name = "LauncherError";
  }
}

function defaultResolve(request) {
  return require.resolve(request);
}

function isFile(file) {
  try {
    return fs.statSync(file).isFile();
  } catch {
    return false;
  }
}

function main(args) {
  let binary;
  try {
    binary = resolveBinary();
  } catch (error) {
    process.stderr.write(`${error.message}\n`);
    process.exit(1);
  }

  run(binary, args);
}

function missingBinaryMessage(expectedPackage, binary) {
  return `schepherd: ${expectedPackage} is installed but its binary ${binary} is missing or not a file.\n${remedy(expectedPackage)}`;
}

function missingPackageMessage(expectedPackage) {
  return (
    `schepherd: the platform package ${expectedPackage} is not installed.\n` +
    `It is an optional dependency of ${ROOT_PACKAGE} and is skipped when optional dependencies are omitted.\n` +
    remedy(expectedPackage)
  );
}

function remedy(expectedPackage) {
  return (
    `Reinstall ${ROOT_PACKAGE} without --omit=optional (or --no-optional) so that ` +
    `${expectedPackage} is installed, or set ${BINARY_OVERRIDE} to the absolute path of a schepherd binary.`
  );
}

function reraise(proc, signal) {
  try {
    proc.kill(proc.pid, signal);
  } catch {
    proc.exit(signalExitCode(signal));
  }

  setTimeout(() => proc.exit(signalExitCode(signal)), RERAISE_GRACE_MS);
}

// Finds the native binary. Every input has a default taken from the running
// process and can be replaced by tests.
function resolveBinary({
  arch = process.arch,
  override = binaryOverride,
  platform = process.platform,
  resolvePackage = defaultResolve,
} = {}) {
  const fromEnvironment = override();
  if (fromEnvironment !== undefined) {
    return fromEnvironment;
  }

  const expectedPackage = packageName(platform, arch);
  if (!isSupported(platform, arch)) {
    throw new LauncherError(unsupportedMessage(platform, arch, expectedPackage));
  }

  let manifest;
  try {
    manifest = resolvePackage(`${expectedPackage}/package.json`);
  } catch {
    throw new LauncherError(missingPackageMessage(expectedPackage));
  }

  const binary = path.join(path.dirname(manifest), "bin", binaryName(platform));
  if (!isFile(binary)) {
    throw new LauncherError(missingBinaryMessage(expectedPackage, binary));
  }

  return binary;
}

// Runs the binary with inherited stdio and mirrors its outcome: the launcher
// exits with the child's status, or on POSIX dies from the same signal. The
// process, platform and spawn function are parameters for tests.
function run(
  binary,
  args,
  { platform = process.platform, proc = process, spawnChild = spawn } = {},
) {
  const windows = platform === "win32";
  const child = spawnChild(binary, args, { stdio: "inherit" });
  const handlers = new Map();

  for (const signal of windows ? WINDOWS_CONSOLE_SIGNALS : FORWARDED_SIGNALS) {
    const handler = windows
      ? () => {}
      : () => {
          if (child.exitCode === null && child.signalCode === null) {
            child.kill(signal);
          }
        };
    handlers.set(signal, handler);
    proc.on(signal, handler);
  }

  const removeHandlers = () => {
    for (const [signal, handler] of handlers) {
      proc.removeListener(signal, handler);
    }
  };

  child.on("error", (error) => {
    removeHandlers();
    proc.stderr.write(`schepherd: cannot start ${binary}: ${error.message}\n`);
    proc.exit(1);
  });

  child.on("exit", (code, signal) => {
    removeHandlers();
    if (signal && !windows) {
      reraise(proc, signal);
      return;
    }
    proc.exit(code ?? (signal ? signalExitCode(signal) : 1));
  });

  return child;
}

function signalExitCode(signal) {
  return 128 + (os.constants.signals[signal] || 0);
}

function unsupportedMessage(platform, arch, expectedPackage) {
  const supported = TARGETS.map((target) => `${target.platform}-${target.arch}`).join(", ");

  return (
    `schepherd: no prebuilt binary for ${platform}-${arch}; the expected package ${expectedPackage} does not exist.\n` +
    `Supported platforms: ${supported}.\n` +
    `Set ${BINARY_OVERRIDE} to the absolute path of a schepherd binary built for this system.`
  );
}

module.exports = {
  LauncherError,
  main,
  resolveBinary,
  run,
};
