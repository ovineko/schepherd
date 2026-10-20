import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

const executable = fileURLToPath(new URL("../index.mjs", import.meta.url));

test("does not claim support for unknown required vocabularies", async () => {
  const root = await mkdtemp(join(tmpdir(), "schepherd-ajv-vocabulary-"));
  try {
    const schema = join(root, "schema.json");
    await writeFile(
      schema,
      JSON.stringify({
        $schema: "https://json-schema.org/draft/2020-12/schema",
        $vocabulary: { "https://example.invalid/vocab/required": true },
        type: "object",
      }),
    );
    const result = spawnSync(process.execPath, [executable, "probe", schema], {
      encoding: "utf8",
      timeout: 10_000,
    });
    assert.ifError(result.error);
    assert.equal(result.status, 0);
    const report = JSON.parse(result.stdout);
    assert.equal(report.supported, false);
    assert.match(report.reason, /vocabulary/i);
  } finally {
    await rm(root, { force: true, recursive: true });
  }
});
