"use strict";

// The only module of the launcher that reads environment variables.

const fs = require("node:fs");
const path = require("node:path");

const BINARY_OVERRIDE = "SCHEPHERD_BINARY";

class EnvError extends Error {
  constructor(message) {
    super(message);
    this.name = "EnvError";
  }
}

// Returns the validated SCHEPHERD_BINARY path, or undefined when the variable
// is unset or empty. The environment is a parameter so tests never have to
// modify process.env.
function binaryOverride(environment = process.env) {
  const value = environment[BINARY_OVERRIDE];
  if (value === undefined || value === "") {
    return undefined;
  }

  if (!path.isAbsolute(value)) {
    throw new EnvError(
      `${BINARY_OVERRIDE} must be an absolute path to the schepherd binary, got "${value}"`,
    );
  }

  let stat;
  try {
    stat = fs.statSync(value);
  } catch (error) {
    throw new EnvError(
      `${BINARY_OVERRIDE} points to "${value}", which cannot be read (${error.code || error.message})`,
    );
  }

  if (!stat.isFile()) {
    throw new EnvError(`${BINARY_OVERRIDE} points to "${value}", which is not a regular file`);
  }

  return value;
}

module.exports = { BINARY_OVERRIDE, binaryOverride, EnvError };
