import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { mkdtemp, mkdir, readFile, realpath, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { after, test } from "node:test";
import { promisify } from "node:util";
import { pathToFileURL } from "node:url";

const source = await readFile(new URL("./preload.mjs", import.meta.url), "utf8");
const execFileAsync = promisify(execFile);
const hooksRoot = await mkdtemp(join(tmpdir(), "helmr-preload-hooks-"));
const hooksPath = join(hooksRoot, "hooks.mjs");
await writeFile(
  hooksPath,
  source
    .replace(
      'import { registerHooks } from "node:module";',
      "const registerHooks = () => {};",
    )
    .replace(/\nregisterHooks\(createHooks\(\)\);\n$/, "\n"),
);
const { createHooks } = await import(pathToFileURL(hooksPath).href);
after(() => rm(hooksRoot, { force: true, recursive: true }));

test("executes imported TypeScript without generated sidecars", async (context) => {
  const root = await realpath(await mkdtemp(join(tmpdir(), "helmr-preload-")));
  context.after(() => rm(root, { force: true, recursive: true }));
  await mkdir(join(root, "pkg"));
  await writeFile(
    join(root, "preload.mjs"),
    source.replace(
      'const programRoot = "/opt/helmr/program";',
      `const programRoot = ${JSON.stringify(root)};`,
    ),
  );
  await writeFile(
    join(root, "pkg", "esm.ts"),
    'enum Value { ESM = "esm" }; export const esmValue: Value = Value.ESM;\n',
  );
  await writeFile(
    join(root, "pkg", "common.cts"),
    'enum Value { Common = "common" }; module.exports = { commonValue: Value.Common };\n',
  );
  await writeFile(
    join(root, "probe.mjs"),
    [
      'import assert from "node:assert/strict";',
      'import { createRequire } from "node:module";',
      'import { esmValue } from "./pkg/esm.ts";',
      'assert.equal(esmValue, "esm");',
      'assert.deepEqual(createRequire(import.meta.url)("./pkg/common.cts"), { commonValue: "common" });',
    ].join("\n"),
  );

  await execFileAsync("node", [
    "--experimental-transform-types",
    "--import",
    pathToFileURL(join(root, "preload.mjs")).href,
    join(root, "probe.mjs"),
  ]);
});

test("delegates executable first-party TypeScript after Node resolves it", () => {
  const hooks = createHooks({
    realpath: () => "/opt/helmr/program/pkg/source.ts",
  });
  const result = hooks.load(
    "file:///opt/helmr/program/link/source.ts",
    { format: "module-typescript" },
    (url, context) => ({ context, url }),
  );
  assert.deepEqual(result, {
    context: { format: "module-typescript" },
    url: "file:///opt/helmr/program/link/source.ts",
  });
});

test("rejects declaration-only first-party modules and delegates dependencies", () => {
  const declarationHooks = createHooks({
    realpath: () => "/opt/helmr/program/pkg/types.d.ts",
  });
  assert.throws(() => declarationHooks.load(
    "file:///opt/helmr/program/pkg/types.d.ts",
    {},
    () => assert.fail("declaration delegated"),
  ), { code: "ERR_HELMR_DECLARATION_MODULE" });

  const dependencyHooks = createHooks({
    realpath: () => "/opt/helmr/program/packages/app/node_modules/pkg/index.js",
  });
  assert.equal(
    dependencyHooks.load(
      "file:///opt/helmr/program/packages/app/node_modules/pkg/index.js",
      {},
      () => "delegated",
    ),
    "delegated",
  );
});
