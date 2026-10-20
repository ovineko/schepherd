import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

const executable = fileURLToPath(new URL("../index.mjs", import.meta.url));

test("supports HTTPS spellings of the registered draft-04, draft-06 and draft-07 dialects", async () => {
  const root = await mkdtemp(join(tmpdir(), "schepherd-ajv-dialects-"));
  try {
    const schema = join(root, "schema.json");
    const input = join(root, "input.json");
    await writeFile(input, "42\n");
    for (const draft of ["04", "06", "07"]) {
      await writeFile(
        schema,
        JSON.stringify({
          $schema: `https://json-schema.org/draft-${draft}/schema#`,
          type: "string",
        }),
      );
      const probe = spawnSync(process.execPath, [executable, "probe", schema], {
        encoding: "utf8",
        timeout: 10_000,
      });
      assert.ifError(probe.error);
      assert.equal(probe.status, 0);
      assert.equal(JSON.parse(probe.stdout).supported, true, draft);
      const validation = spawnSync(process.execPath, [executable, "validate", schema, input], {
        encoding: "utf8",
        timeout: 10_000,
      });
      assert.ifError(validation.error);
      assert.equal(validation.status, 1);
      assert.equal(JSON.parse(validation.stdout).errors[0].keyword, "type");
    }
  } finally {
    await rm(root, { force: true, recursive: true });
  }
});
