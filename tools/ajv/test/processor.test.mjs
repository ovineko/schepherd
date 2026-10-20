import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

const entrypoint = fileURLToPath(new URL("../index.mjs", import.meta.url));

async function fixture(run) {
  const root = await mkdtemp(join(tmpdir(), "schepherd-ajv-"));
  try {
    const schema = join(root, "schema.json");
    const input = join(root, "literal-[input].json");
    await run({ input, root, schema });
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

test("reports an invalid literal-path document without modifying it", async () => {
  await fixture(async ({ input, schema }) => {
    await writeFile(
      schema,
      JSON.stringify({
        properties: { count: { type: "integer" } },
        required: ["count"],
        type: "object",
      }),
    );
    const bytes = '{"count":"not an integer"}\n';
    await writeFile(input, bytes);
    assert.deepEqual(invoke("probe", schema), { code: 0, report: { supported: true } });
    const result = invoke("validate", schema, input);
    assert.equal(result.code, 1);
    assert.equal(result.report.valid, false);
    assert.equal(result.report.file, input);
    assert.ok(
      result.report.errors.some(
        (error) => error.keyword === "type" && error.instancePath === "/count",
      ),
    );
    assert.equal(await readFile(input, "utf8"), bytes);
  });
});

test("does not coerce or remove input properties to make validation pass", async () => {
  await fixture(async ({ input, schema }) => {
    await writeFile(
      schema,
      JSON.stringify({
        additionalProperties: false,
        properties: { count: { default: 1, type: "integer" } },
        type: "object",
      }),
    );
    await writeFile(input, '{"extra":true}\n');
    const result = invoke("validate", schema, input);
    assert.equal(result.code, 1);
    assert.ok(result.report.errors.some((error) => error.keyword === "additionalProperties"));
  });
});

test("selects draft-04 and rejects external reference retrieval during probing", async () => {
  await fixture(async ({ input, schema }) => {
    await writeFile(
      schema,
      JSON.stringify({
        $schema: "http://json-schema.org/draft-04/schema#",
        exclusiveMinimum: true,
        minimum: 0,
        type: "integer",
      }),
    );
    await writeFile(input, "0\n");
    assert.equal(invoke("probe", schema).report.supported, true);
    assert.equal(invoke("validate", schema, input).code, 1);
    await writeFile(schema, JSON.stringify({ $ref: "https://example.invalid/never-fetch.json" }));
    const result = invoke("probe", schema);
    assert.equal(result.code, 0);
    assert.equal(result.report.supported, false);
    assert.match(result.report.reason, /reference/i);
  });
});

test("distinguishes malformed schema and malformed input from schema incompatibility", async () => {
  await fixture(async ({ input, schema }) => {
    await writeFile(schema, '{"type":');
    const badSchema = invoke("probe", schema);
    assert.equal(badSchema.code, 4);
    assert.equal(badSchema.report.kind, "schema");
    await writeFile(schema, '{"type":"object"}\n');
    await writeFile(input, '{"broken":');
    const badInput = invoke("validate", schema, input);
    assert.equal(badInput.code, 6);
    assert.equal(badInput.report.kind, "input");
    assert.equal(badInput.report.file, input);
  });
});

test("refuses asynchronous schemas instead of treating a Promise as validation success", async () => {
  await fixture(async ({ schema }) => {
    await writeFile(schema, JSON.stringify({ $async: true, type: "object" }));
    const result = invoke("probe", schema);
    assert.equal(result.code, 0);
    assert.equal(result.report.supported, false);
    assert.match(result.report.reason, /asynchronous/i);
  });
});
