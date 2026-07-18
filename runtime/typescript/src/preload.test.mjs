import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { test } from "node:test";

const source = await readFile(new URL("./preload.mjs", import.meta.url), "utf8");
const moduleURL = `data:text/javascript;base64,${
  Buffer.from(source.replace(/\ninstallHooks\(\);\n$/, "\n")).toString("base64")
}`;
const { createHooks } = await import(moduleURL);

function notFound() {
  const error = new Error("not found");
  error.code = "ERR_MODULE_NOT_FOUND";
  return error;
}

function unsupportedDirectory() {
  const error = new Error("directory import");
  error.code = "ERR_UNSUPPORTED_DIR_IMPORT";
  return error;
}

test("uses the ESM and CommonJS candidate orders after Node fails", () => {
  const hooks = createHooks({ modules: [] });
  for (const conditions of [["import"], new Set(["require"])]) {
    const commonjs = typeof conditions.has === "function"
      ? conditions.has("require")
      : conditions.includes("require");
    const seen = [];
    const result = hooks.resolve(
      "./tool",
      {
        conditions,
        parentURL: "file:///opt/helmr/program/pkg/main.js",
      },
      (specifier) => {
        seen.push(specifier);
        const accepted = commonjs
          ? "/opt/helmr/program/pkg/tool.js"
          : "file:///opt/helmr/program/pkg/tool.js";
        if (specifier === accepted) {
          return {
            url: commonjs ? `file://${specifier}` : specifier,
          };
        }
        throw notFound();
      },
    );
    assert.equal(result.url, "file:///opt/helmr/program/pkg/tool.js");
    assert.deepEqual(
      seen.slice(0, 4),
      commonjs
        ? ["./tool",
          "/opt/helmr/program/pkg/tool.ts",
          "/opt/helmr/program/pkg/tool.cts",
          "/opt/helmr/program/pkg/tool.js"]
        : ["./tool",
          "file:///opt/helmr/program/pkg/tool.ts",
          "file:///opt/helmr/program/pkg/tool.mts",
          "file:///opt/helmr/program/pkg/tool.js"],
    );
  }
});

test("tries index candidates after Node rejects a directory import", () => {
  const hooks = createHooks({ modules: [] });
  const seen = [];
  const result = hooks.resolve(
    "./tool",
    {
      conditions: ["import"],
      parentURL: "file:///opt/helmr/program/pkg/main.js",
    },
    (specifier) => {
      seen.push(specifier);
      if (specifier === "./tool") {
        throw unsupportedDirectory();
      }
      if (specifier === "file:///opt/helmr/program/pkg/tool/index.ts") {
        return { url: specifier };
      }
      throw notFound();
    },
  );
  assert.equal(
    result.url,
    "file:///opt/helmr/program/pkg/tool/index.ts",
  );
  assert.equal(seen.at(-1), result.url);
});

test("does not rewrite bare, exact-extension, or dependency specifiers", () => {
  const hooks = createHooks({ modules: [] });
  for (const specifier of [
    "package",
    "./exact.ts",
    "file:///opt/helmr/program/node_modules/pkg/tool",
  ]) {
    let calls = 0;
    assert.throws(() => hooks.resolve(
      specifier,
      {
        conditions: ["import"],
        parentURL: "file:///opt/helmr/program/main.js",
      },
      () => {
        calls++;
        throw notFound();
      },
    ), { code: "ERR_MODULE_NOT_FOUND" });
    assert.equal(calls, 1);
  }
});

test("loads mapped sidecars while preserving the original URL", () => {
  const hooks = createHooks({
    modules: [{
      codePath: "helmr/files/modules/a.mjs",
      format: "module",
      path: "pkg/source.ts",
    }],
  }, {
    readFile: (path) => Buffer.from(`sidecar:${path}`),
    realpath: () => "/opt/helmr/program/pkg/source.ts",
  });
  const result = hooks.load(
    "file:///opt/helmr/program/link/source.ts",
    {},
    () => assert.fail("mapped module delegated"),
  );
  assert.equal(result.format, "module");
  assert.equal(result.shortCircuit, true);
  assert.match(result.source.toString(), /helmr\/files\/modules\/a\.mjs$/);
});

test("rejects declaration-only first-party modules and delegates dependencies", () => {
  const declarationHooks = createHooks({ modules: [] }, {
    realpath: () => "/opt/helmr/program/pkg/types.d.ts",
  });
  assert.throws(() => declarationHooks.load(
    "file:///opt/helmr/program/pkg/types.d.ts",
    {},
    () => assert.fail("declaration delegated"),
  ), { code: "ERR_HELMR_DECLARATION_MODULE" });

  const dependencyHooks = createHooks({ modules: [] }, {
    realpath: () => "/opt/helmr/program/node_modules/pkg/index.js",
  });
  assert.equal(
    dependencyHooks.load(
      "file:///opt/helmr/program/node_modules/pkg/index.js",
      {},
      () => "delegated",
    ),
    "delegated",
  );
});
