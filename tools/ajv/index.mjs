#!/usr/bin/env node
import Ajv from "ajv";
import AjvDraft04 from "ajv-draft-04";
import addFormats from "ajv-formats";
import Ajv2019 from "ajv/dist/2019.js";
import Ajv2020 from "ajv/dist/2020.js";
import draft06 from "ajv/dist/refs/json-schema-draft-06.json" with { type: "json" };
import standaloneCode from "ajv/dist/standalone/index.js";
import { createHash } from "node:crypto";
import { readFileSync, statSync } from "node:fs";
import { createRequire } from "node:module";
import { resolve } from "node:path";
import { Script } from "node:vm";

const frontendRequire = createRequire(import.meta.url);
const MAX_DOCUMENT_BYTES = 64 * 1024 * 1024;
const DIALECTS = new Map([
  ["http://json-schema.org/draft-04/schema", AjvDraft04],
  ["http://json-schema.org/draft-06/schema", Ajv],
  ["http://json-schema.org/draft-07/schema", Ajv],
  ["https://json-schema.org/draft/2019-09/schema", Ajv2019],
  ["https://json-schema.org/draft/2020-12/schema", Ajv2020],
]);
const VOCABULARIES = new Set(
  [
    [
      "2020-12",
      [
        "core",
        "applicator",
        "unevaluated",
        "validation",
        "meta-data",
        "format-annotation",
        "content",
      ],
    ],
    ["2019-09", ["core", "applicator", "validation", "meta-data", "format", "content"]],
  ].flatMap(([draft, names]) =>
    names.map((name) => `https://json-schema.org/draft/${draft}/vocab/${name}`),
  ),
);
const options = {
  allErrors: true,
  coerceTypes: false,
  logger: false,
  ownProperties: true,
  removeAdditional: false,
  strict: false,
  useDefaults: false,
  validateFormats: false,
  validateSchema: true,
};

function artifactError(path, cause) {
  const error = new Error(cause instanceof Error ? cause.message : String(cause));
  error.kind = "resource";
  error.file = path;
  error.code = 7;
  return error;
}

function compile(schema, standalone = false) {
  if (
    schema !== null &&
    typeof schema === "object" &&
    !Array.isArray(schema) &&
    schema.$async === true
  ) {
    return unsupported("asynchronous schemas are not supported by this processor");
  }
  const dialect = typeof schema === "object" && schema !== null ? schema.$schema : undefined;
  const dialectId =
    typeof dialect === "string"
      ? dialect
          .replace(
            /^https:\/\/json-schema\.org\/(draft-0[467]\/schema)#?$/,
            "http://json-schema.org/$1",
          )
          .replace(/#$/, "")
      : dialect;
  const Constructor = dialect === undefined ? Ajv : DIALECTS.get(dialectId);
  if (!Constructor) {
    return unsupported(`unsupported schema dialect: ${String(dialect)}`);
  }
  if (schema && typeof schema === "object" && schema.$vocabulary) {
    for (const [uri, required] of Object.entries(schema.$vocabulary)) {
      if (required === true && !VOCABULARIES.has(uri)) {
        return unsupported(`unsupported required vocabulary: ${uri}`);
      }
    }
  }
  const validator = new Constructor(standalone ? { ...options, code: { source: true } } : options);
  if (dialectId === "http://json-schema.org/draft-06/schema") {
    validator.addMetaSchema(draft06);
  }
  if (typeof dialect === "string" && dialect.startsWith("https://json-schema.org/draft-0")) {
    validator.addMetaSchema(validator.getSchema(dialectId)?.schema, dialect);
  }
  addFormats(validator);
  try {
    if (!validator.validateSchema(schema)) {
      const error = new Error(validator.errorsText(validator.errors));
      error.kind = "schema";
      error.code = 4;
      throw error;
    }
    const validate = validator.compile(schema);
    if (validate.$async) {
      return unsupported("asynchronous schemas are not supported by this processor");
    }
    return {
      supported: true,
      validate,
      ...(standalone ? { template: standaloneCode(validator, validate) } : {}),
    };
  } catch (error) {
    if (error?.missingRef) {
      return unsupported(
        `unresolved reference; network retrieval is disabled: ${String(error.missingRef)}`,
      );
    }
    if (error?.kind === "schema") {
      throw error;
    }
    const invalid = new Error(
      `schema cannot be compiled by AJV: ${error instanceof Error ? error.message : String(error)}`,
    );
    invalid.kind = "schema";
    invalid.code = 4;
    throw invalid;
  }
}

function emit(document, code) {
  process.stdout.write(JSON.stringify(document) + "\n");
  process.exitCode = code;
}

function loadCompiled(path) {
  try {
    const stat = statSync(path);
    if (!stat.isFile() || stat.size > MAX_DOCUMENT_BYTES) {
      throw new Error("expected a regular compiled artifact no larger than 64 MiB");
    }
    const source = readFileSync(path, "utf8");
    const module = { exports: {} };
    // Caller-verified compiler output gets only CommonJS bindings, not ambient Node globals or eval.
    // eslint-disable-next-line sonarjs/code-eval -- Executes only the explicit standalone compiler artifact; Schepherd verifies its recipe and SHA-256 before passing it.
    new Script(source, { filename: "schepherd-ajv-compiled.cjs" }).runInNewContext(
      { exports: module.exports, module, require: frontendRequire },
      { contextCodeGeneration: { strings: false, wasm: false } },
    );
    const validate = module.exports;
    if (
      typeof validate !== "function" ||
      validate.$async ||
      validate.constructor?.name === "AsyncFunction"
    ) {
      throw new Error("compiled artifact must export a synchronous validator");
    }
    return validate;
  } catch (cause) {
    throw artifactError(path, cause);
  }
}

function main(args) {
  const [action, schemaName, inputName, flag, artifactName] = args;
  if (action === "identity" && args.length === 1) {
    emit(
      {
        dependencies: Object.fromEntries(
          ["ajv", "ajv-draft-04", "ajv-formats"].map((name) => [
            name,
            frontendRequire(`${name}/package.json`).version,
          ]),
        ),
        frontend: {
          sha256: createHash("sha256")
            .update(readFileSync(new URL(import.meta.url)))
            .digest("hex"),
        },
        runtime: { abi: process.versions.modules, name: "node", version: process.version },
      },
      0,
    );
    return;
  }
  const compiledValidation =
    action === "validate" && args.length === 5 && flag === "--compiled" && Boolean(artifactName);
  const capabilityRequest = (action === "probe" || action === "compile") && args.length === 2;
  const validationRequest =
    action === "validate" && inputName && (args.length === 3 || compiledValidation);
  if (!schemaName || (!capabilityRequest && !validationRequest)) {
    emit(
      {
        error:
          "Usage: schepherd-ajv identity | probe <schema.json> | compile <schema.json> | validate <schema.json> <input.json> [--compiled <template-path>]",
        kind: "usage",
      },
      2,
    );
    return;
  }
  const schemaPath = resolve(schemaName);
  const schema = readJson(schemaPath, "schema");
  const compiled = compiledValidation
    ? { supported: true, validate: loadCompiled(resolve(artifactName)) }
    : compile(schema, action === "compile");
  if (action === "probe" || action === "compile") {
    emit(
      compiled.supported
        ? { supported: true, ...(action === "compile" ? { template: compiled.template } : {}) }
        : compiled,
      0,
    );
    return;
  }
  if (!compiled.supported) {
    emit({ error: compiled.reason, file: schemaPath, kind: "schema" }, 3);
    return;
  }
  const file = resolve(inputName);
  const input = readJson(file, "input");
  let valid;
  try {
    valid = compiled.validate(input);
    if (typeof valid !== "boolean") {
      throw new Error("compiled artifact did not return a validation boolean");
    }
  } catch (cause) {
    if (compiledValidation) {
      throw artifactError(resolve(artifactName), cause);
    }
    throw cause;
  }
  emit({ errors: compiled.validate.errors ?? [], file, valid }, valid ? 0 : 1);
}

function readJson(path, kind) {
  try {
    const stat = statSync(path);
    if (!stat.isFile() || stat.size > MAX_DOCUMENT_BYTES) {
      throw new Error("expected a regular JSON file no larger than 64 MiB");
    }
    return JSON.parse(readFileSync(path, "utf8"));
  } catch (cause) {
    const error = new Error(cause instanceof Error ? cause.message : String(cause));
    error.kind = kind;
    error.file = path;
    error.code = kind === "schema" ? 4 : 6;
    throw error;
  }
}

function unsupported(reason) {
  return { reason, supported: false };
}

try {
  main(process.argv.slice(2));
} catch (error) {
  emit(
    {
      error: error instanceof Error ? error.message : String(error),
      kind: error?.kind ?? "resource",
      ...(error?.file ? { file: error.file } : {}),
    },
    error?.code ?? 7,
  );
}
