import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { chmod, mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { createRequire } from "node:module";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

const entrypoint = fileURLToPath(new URL("../index.mjs", import.meta.url));
const frontendRequire = createRequire(new URL("../index.mjs", import.meta.url));

async function compile(schema, artifact) {
  const result = invoke("compile", schema);
  assert.equal(result.code, 0);
  assert.equal(result.report.supported, true);
  assert.equal(typeof result.report.template, "string");
  assert.ok(result.report.template.length > 0);
  await writeFile(artifact, result.report.template);
  return result.report.template;
}

async function fixture(run) {
  const root = await mkdtemp(join(tmpdir(), "schepherd-ajv-compiled-"));
  try {
    await run({
      artifact: join(root, "compiled.blob"),
      input: join(root, "literal-[input].json"),
      root,
      schema: join(root, "schema.json"),
    });
  } finally {
    await rm(root, { force: true, recursive: true });
  }
}

function invoke(...args) {
  const result = spawnSync(process.execPath, [entrypoint, ...args], {
    encoding: "utf8",
    env: { ...process.env, HTTP_PROXY: "http://127.0.0.1:9", HTTPS_PROXY: "http://127.0.0.1:9" },
    timeout: 10_000,
  });
  assert.ifError(result.error);
  assert.equal(result.stderr, "");
  return { code: result.status, report: JSON.parse(result.stdout) };
}

test("standalone validators preserve exact ordinary validation reports for every dialect", async () => {
  await fixture(async ({ artifact, input, schema }) => {
    for (const dialect of [
      undefined,
      "http://json-schema.org/draft-04/schema#",
      "https://json-schema.org/draft-04/schema#",
      "http://json-schema.org/draft-06/schema#",
      "https://json-schema.org/draft-06/schema#",
      "http://json-schema.org/draft-07/schema#",
      "https://json-schema.org/draft-07/schema#",
      "https://json-schema.org/draft/2019-09/schema",
      "https://json-schema.org/draft/2020-12/schema",
    ]) {
      await writeFile(
        schema,
        JSON.stringify({
          $schema: dialect,
          additionalProperties: false,
          properties: {
            count: { default: 1, type: "integer" },
            name: { minLength: 2, type: "string" },
          },
          required: ["count", "name"],
          type: "object",
        }),
      );
      await compile(schema, artifact);
      for (const document of [
        '{"count":1,"name":"ok"}',
        '{"count":"1","name":"x","extra":true}',
        '{"name":"ok"}',
      ]) {
        await writeFile(input, document);
        const ordinary = invoke("validate", schema, input);
        const compiled = invoke("validate", schema, input, "--compiled", artifact);
        assert.deepEqual(compiled, ordinary, `${dialect}: ${document}`);
        assert.equal(compiled.code, document.includes('"count":1') ? 0 : 1);
        assert.equal(compiled.report.file, input);
        assert.equal(await readFile(input, "utf8"), document);
      }
    }
  });
});

test("standalone compilation is deterministic and runtime dependencies resolve at the frontend", async () => {
  await fixture(async ({ artifact, input, root, schema }) => {
    const document = JSON.stringify({
      properties: {
        items: { type: "array", uniqueItems: true },
        text: { minLength: 2, type: "string" },
      },
      type: "object",
    });
    await writeFile(schema, document);
    const template = await compile(schema, artifact);
    const relocated = join(root, "relocated");
    await mkdir(relocated);
    const otherSchema = join(relocated, "schema.json");
    const otherArtifact = join(relocated, "artifact.blob");
    await writeFile(otherSchema, document);
    assert.equal(await compile(otherSchema, otherArtifact), template);
    assert.ok(!template.includes(root));
    const impostor = join(relocated, "node_modules", "ajv", "dist", "runtime");
    await mkdir(impostor, { recursive: true });
    for (const helper of ["ucs2length", "equal"]) {
      await writeFile(join(impostor, `${helper}.js`), 'throw new Error("wrong dependency root");');
    }
    await chmod(otherArtifact, 0o444);
    await writeFile(input, '{"items":[{"x":1},{"x":1}],"text":"x"}');
    assert.deepEqual(
      invoke("validate", otherSchema, input, "--compiled", otherArtifact),
      invoke("validate", otherSchema, input),
    );
    await writeFile(input, '{"items":[{"x":1},{"x":2}],"text":"ok"}');
    assert.equal(invoke("validate", otherSchema, input, "--compiled", otherArtifact).code, 0);
  });
});

test("standalone validators support recursive local references without retrieving remote schemas", async () => {
  await fixture(async ({ artifact, input, root, schema }) => {
    await writeFile(
      schema,
      JSON.stringify({
        properties: { next: { $ref: "#" }, value: { type: "integer" } },
        type: "object",
      }),
    );
    await compile(schema, artifact);
    await writeFile(input, '{"next":{"value":"bad"}}');
    assert.deepEqual(
      invoke("validate", schema, input, "--compiled", artifact),
      invoke("validate", schema, input),
    );
    await writeFile(join(root, "never-fetch.json"), '{"type":"integer"}');
    for (const unsupported of [
      { $ref: "https://example.invalid/never-fetch.json" },
      { $ref: "./never-fetch.json" },
      { $async: true, type: "object" },
      { $schema: "https://example.invalid/dialect" },
      { $vocabulary: { "https://example.invalid/vocabulary": true } },
    ]) {
      await writeFile(schema, JSON.stringify(unsupported));
      const result = invoke("compile", schema);
      assert.deepEqual(result, invoke("probe", schema));
      assert.equal(result.code, 0);
      assert.equal(result.report.supported, false);
      assert.equal(result.report.template, undefined);
    }
  });
});

test("compiled validation classifies missing, malformed and non-validator artifacts as resource errors", async () => {
  await fixture(async ({ artifact, input, root, schema }) => {
    await writeFile(schema, '{"type":"object"}');
    await writeFile(input, "{}");
    for (const source of [
      undefined,
      "not javascript {{{",
      "",
      "module.exports = {};",
      "module.exports = async () => true;",
      "module.exports = () => Promise.resolve(true);",
    ]) {
      if (source !== undefined) await writeFile(artifact, source);
      const result = invoke("validate", schema, input, "--compiled", artifact);
      assert.equal(result.code, 7);
      assert.equal(result.report.kind, "resource");
      assert.equal(result.report.file, artifact);
      assert.equal(result.report.valid, undefined);
    }
    const directory = invoke("validate", schema, input, "--compiled", root);
    assert.equal(directory.code, 7);
    assert.equal(directory.report.kind, "resource");
  });
});

test("standalone execution exposes no ambient Node globals or further string compilation", async () => {
  await fixture(async ({ artifact, input, schema }) => {
    await writeFile(schema, '{"type":"object"}');
    await writeFile(input, "{}");
    await writeFile(
      artifact,
      'module.exports = () => typeof process === "undefined" && typeof fetch === "undefined";',
    );
    assert.equal(invoke("validate", schema, input, "--compiled", artifact).code, 0);
    await writeFile(artifact, 'module.exports = () => eval("true");');
    const result = invoke("validate", schema, input, "--compiled", artifact);
    assert.equal(result.code, 7);
    assert.equal(result.report.kind, "resource");
    assert.equal(result.report.file, artifact);
  });
});

test("compiled validation preserves malformed JSON errors and the original input path", async () => {
  await fixture(async ({ artifact, input, schema }) => {
    await writeFile(schema, '{"type":"object"}');
    await compile(schema, artifact);
    await writeFile(input, '{"broken":');
    const result = invoke("validate", schema, input, "--compiled", artifact);
    assert.deepEqual(result, invoke("validate", schema, input));
    assert.equal(result.code, 6);
    assert.equal(result.report.file, input);
    await writeFile(schema, '{"type":');
    assert.deepEqual(invoke("compile", schema), invoke("probe", schema));
    await writeFile(schema, '{"type":"not-a-type"}');
    const badSchema = invoke("compile", schema);
    assert.deepEqual(badSchema, invoke("probe", schema));
    assert.equal(badSchema.code, 4);
    assert.equal(badSchema.report.kind, "schema");
  });
});

test("standalone boolean schemas retain valid and invalid reports", async () => {
  await fixture(async ({ artifact, input, schema }) => {
    await writeFile(input, "null");
    for (const value of [true, false]) {
      await writeFile(schema, JSON.stringify(value));
      await compile(schema, artifact);
      const result = invoke("validate", schema, input, "--compiled", artifact);
      assert.deepEqual(result, invoke("validate", schema, input));
      assert.equal(result.code, value ? 0 : 1);
    }
  });
});

test("schema and document strings are data rather than executable artifact sources", async () => {
  await fixture(async ({ artifact, input, schema }) => {
    const text = '"; throw new Error("untrusted data executed"); //';
    await writeFile(schema, JSON.stringify({ const: text, type: "string" }));
    await compile(schema, artifact);
    await writeFile(input, JSON.stringify(text));
    assert.equal(invoke("validate", schema, input, "--compiled", artifact).code, 0);
    assert.equal(invoke("validate", schema, input, "--compiled", input).code, 7);
    assert.equal(invoke("validate", schema, input, "--compiled", schema).code, 7);
  });
});

test("identity contains exact dependencies, runtime and frontend digest without paths", async () => {
  const result = invoke("identity");
  assert.equal(result.code, 0);
  assert.deepEqual(result, invoke("identity"));
  assert.deepEqual(result.report, {
    dependencies: Object.fromEntries(
      ["ajv", "ajv-draft-04", "ajv-formats"].map((name) => [
        name,
        frontendRequire(`${name}/package.json`).version,
      ]),
    ),
    frontend: {
      sha256: createHash("sha256")
        .update(await readFile(entrypoint))
        .digest("hex"),
    },
    runtime: { abi: process.versions.modules, name: "node", version: process.version },
  });
  assert.ok(!JSON.stringify(result.report).includes(entrypoint));
});

test("rejects malformed compiled and identity command arguments", () => {
  for (const args of [
    ["identity", "extra"],
    ["compile"],
    ["validate", "schema", "input", "--compiled"],
    ["validate", "schema", "input", "--other", "artifact"],
  ]) {
    const result = invoke(...args);
    assert.equal(result.code, 2);
    assert.equal(result.report.kind, "usage");
  }
});
