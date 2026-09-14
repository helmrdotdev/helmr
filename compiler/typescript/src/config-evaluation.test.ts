import { afterAll, expect, test } from "bun:test"
import { readFileSync } from "node:fs"
import { spawnSync } from "node:child_process"
import { mkdir, mkdtemp, readFile, realpath, rm, symlink, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { dirname, resolve } from "node:path"
import { pathToFileURL } from "node:url"
import { compileConfig, compileProgram } from "./bundle"

const cleanup: string[] = []
afterAll(async () => { await Promise.all(cleanup.map((p) => rm(p, { recursive: true, force: true }))) })
async function fixture() {
  const root = await realpath(await mkdtemp(resolve(tmpdir(), "helmr-config-origin-")))
  cleanup.push(root)
  await put(root, "package.json", '{"type":"module"}')
  await mkdir(resolve(root, "output"))
  return root
}
async function put(root: string, path: string, contents: string) {
  await mkdir(dirname(resolve(root, path)), { recursive: true })
  await writeFile(resolve(root, path), contents)
}
async function evaluate(root: string) {
  const compiled = await compileConfig({ root, outputRoot: resolve(root, "output"), nodeVersion: "24.19.0" })
  try {
    const result = spawnSync("node", ["--no-strip-types", "--enable-source-maps", "--input-type=module", "-e",
      `console.log(JSON.stringify((await import(${JSON.stringify(pathToFileURL(compiled.path).href)})).default))`], { encoding: "utf8" })
    return { ...result, code: await readFile(compiled.path, "utf8"), map: await readFile(`${compiled.path}.map`, "utf8") }
  } finally { await compiled.cleanup() }
}

test.each(["sub", "node_modules/copied"])("config origin preserves assets, CJS scopes and native installed export conditions in %s", async (slot) => {
  const root = await fixture()
  await put(root, `${slot}/asset.txt`, "source-asset")
  await put(root, `${slot}/data.cjs`, 'module.exports="dynamic-data"')
  await put(root, `${slot}/helper.cts`, 'module.exports=require("node:fs").readFileSync(__dirname+"/asset.txt","utf8")')
  await put(root, "node_modules/dual/package.json", '{"type":"module","exports":{"import":"./index.mjs","require":"./index.cjs"}}')
  await put(root, "node_modules/dual/asset.txt", "installed-asset")
  await put(root, "node_modules/dual/index.mjs", 'import {readFileSync} from "node:fs";export default readFileSync(new URL("asset.txt",import.meta.url),"utf8")')
  await put(root, "node_modules/dual/index.cjs", 'module.exports="required"')
  await put(root, `${slot}/helper.ts`, `
    import {readFileSync} from "node:fs";
    import {createRequire} from "node:module";
    import cjs from "./helper.cts";
    import installed from "dual";
    const localRequire=createRequire(import.meta.url), file="asset.txt";
    function shadow(__dirname:string){return __dirname}
    export default [readFileSync(new URL(file,import.meta.url),"utf8"), cjs,
      localRequire("./data.cjs"), require.resolve("./data.cjs"), installed, require("dual"),
      import.meta.dirname, import.meta.filename, shadow("local")];
  `)
  await put(root, "helmr.config.ts", `export {default} from "./${slot}/helper.ts"`)
  const first = await evaluate(root), second = await evaluate(root)
  expect(first.status).toBe(0)
  expect(JSON.parse(first.stdout)).toEqual(["source-asset", "source-asset", "dynamic-data", resolve(root, `${slot}/data.cjs`), "installed-asset", "required", resolve(root, slot), resolve(root, `${slot}/helper.ts`), "local"])
  expect(second.code).toBe(first.code)
  expect(second.map).toBe(first.map)
  expect(first.code).not.toContain("readFileSync(new URL(\"asset.txt\"")
  await expect(readFile(resolve(root, "output/config/config-evaluation.mjs"))).rejects.toThrow()
})

test.each(["sub", "node_modules/copied"])("config retains program esbuild tsconfig/JSX/class semantics for %s", async (slot) => {
  const root = await fixture()
  await put(root, "tsconfig.json", '{"compilerOptions":{"jsxFactory":"rootFactory","useDefineForClassFields":true}}')
  await put(root, `${slot}/package.json`, '{"type":"module"}')
  await put(root, `${slot}/tsconfig.json`, '{"compilerOptions":{"jsxFactory":"localFactory","useDefineForClassFields":false,"baseUrl":".","paths":{"@alias/*":["own/*"]}}}')
  await put(root, `${slot}/own/value.ts`, 'export const value="nested"')
  await put(root, `${slot}/helper.tsx`, `
    const rootFactory=()=>"root",localFactory=()=>"local",React={createElement:()=>"default"};
    const effects:string[]=[];
    class Base{set value(v:number){effects.push("setter")}}
    class Child extends Base{value=1}
    new Child();export default [<div/>,effects];
  `)
  await put(root, "helmr.config.ts", `export {default} from "./${slot}/helper.tsx"`)
  const config = await evaluate(root)
  expect(config.status).toBe(0)
  expect(JSON.parse(config.stdout)).toEqual(slot === "sub" ? ["local", ["setter"]] : ["default", []])
  if (slot === "sub") {
    await put(root, `${slot}/helper.tsx`, 'import {value} from "@alias/value.js";export default value')
    expect(JSON.parse((await evaluate(root)).stdout)).toBe("nested")
  }
})

test.each(["sub", "node_modules/copied"])("config throws once with original source positions for %s", async (slot) => {
  const root = await fixture()
  await put(root, `${slot}/helper.ts`, 'globalThis.count=(globalThis.count??0)+1;throw Error("count="+globalThis.count);\nexport default null;')
  await put(root, "helmr.config.ts", `export {default} from "./${slot}/helper.ts"`)
  const result = await evaluate(root)
  expect(result.status).toBe(1)
  expect(result.stderr).toContain("Error: count=1")
  expect(result.stderr).toContain(`${slot}/helper.ts:1`)
})

test.each([
  'const p="./x.js";export default require(p)',
  'const r=require;export default r("./x.js")',
  'const p="./x.js";export default await import(p)',
])("config fails unsupported module loading explicitly: %s", async (source) => {
  const root = await fixture()
  await put(root, "helmr.config.ts", source)
  await expect(evaluate(root)).rejects.toThrow("will not be bundled")
})

test("compiled config import.meta.resolve fails instead of resolving from temporary output", async () => {
  const root = await fixture()
  await put(root, "helmr.config.ts", 'const {resolve}=import.meta;export default resolve("./x.js")')
  const result = await evaluate(root)
  expect(result.status).toBe(1)
  expect(result.stderr).toContain("import.meta.resolve() is unsupported")
})

test.each(["direct", "transitive", "alias", "symlink"])("program rejects %s imports of build-only root config", async (kind) => {
  const root = await fixture()
  await put(root, "helmr.config.ts", 'throw Error("CONFIG EXECUTED");export default {}')
  let specifier = "../helmr.config.ts"
  if (kind === "transitive") { await put(root, "shared.ts", 'export {default} from "./helmr.config.ts"'); specifier = "../shared.ts" }
  if (kind === "alias") { await put(root, "tsconfig.json", '{"compilerOptions":{"baseUrl":".","paths":{"config":["helmr.config.ts"]}}}'); specifier = "config" }
  if (kind === "symlink") { await symlink("helmr.config.ts", resolve(root, "linked.ts")); specifier = "../linked.ts" }
  await put(root, "tasks/task.ts", `import config from ${JSON.stringify(specifier)};export const value=config;`)
  await expect(compileProgram({ root, outputRoot: resolve(root, "output"), architecture: "x86_64", nodeVersion: "24.19.0", config: { dirs: ["tasks"], ignorePatterns: [], compilePackages: [] } })).rejects.toThrow("build-only")
})

test("config rejects source and tsconfig escapes before evaluation", async () => {
  const root = await fixture(), outside = await fixture()
  await put(outside, "helper.ts", "export default 1")
  await symlink(resolve(outside, "helper.ts"), resolve(root, "escape.ts"))
  await put(root, "helmr.config.ts", 'export {default} from "./escape.ts"')
  await expect(evaluate(root)).rejects.toThrow("escapes submitted source")
  await put(root, "helmr.config.ts", 'export default {}')
  await put(outside, "base.json", "{}")
  await put(root, "tsconfig.json", JSON.stringify({ extends: resolve(outside, "base.json") }))
  await expect(evaluate(root)).rejects.toThrow("tsconfig path escapes project")
})

// Exercise the real subprocess/framing/cleanup path, not only direct imports.
test("generated evaluator canonicalizes computed selection and reports original throw locations", async () => {
  const root = await fixture()
  const toolchain = await fixture()
  const evaluator = resolve(toolchain, "config-evaluator.mjs")
  await writeFile(evaluator, await readFile(resolve(import.meta.dirname, "../../../internal/compiler/config-evaluator.mjs")))
  await mkdir(resolve(toolchain, "node_modules"))
  await symlink(await realpath(resolve(import.meta.dirname, "../node_modules/esbuild")), resolve(toolchain, "node_modules/esbuild"))
  const run = () => {
    const framePath = resolve(toolchain, "frame")
    // Shell owns fd 3: Bun's node:child_process only returns the standard pipes.
    const result = spawnSync("bash", ["-c", 'exec "$@" 3>"$HELMR_TEST_FRAME"', "helmr-config-test",
      "node", "--no-strip-types", "--enable-source-maps", evaluator, root, "24.19.0", resolve(root, "output")], {
      encoding: "utf8", env: { ...process.env, HELMR_TEST_FRAME: framePath },
    })
    return { ...result, frame: readFileSync(framePath) }
  }

  await put(root, "node_modules/helper/package.json", '{"type":"module","exports":"./index.ts"}')
  await put(root, "node_modules/helper/index.ts", 'export const selector="node_modules/helper"')
  await put(root, "helmr.config.ts", `
    import {appendFileSync} from "node:fs";
    import {selector} from "helper";
    appendFileSync(new URL("./count",import.meta.url),"once");
    const config={compilePackages:[selector],dirs:["tasks"]};export default config;
  `)
  const success = run()
  expect(success.stderr).toBe("")
  expect(success.status).toBe(0)
  const frame = success.frame
  expect(frame.readUInt32BE(0)).toBe(frame.byteLength - 4)
  expect(frame.subarray(4).toString()).toBe('{"compilePackages":["node_modules/helper"],"dirs":["tasks"],"ignorePatterns":[]}')
  expect(await readFile(resolve(root, "count"), "utf8")).toBe("once")
  await put(root, "node_modules/helper/index.ts", 'throw Error("evaluator-origin");export const selector="unused"')
  const failure = run()
  expect(failure.status).toBe(1)
  expect(failure.stderr).toContain("node_modules/helper/index.ts:1")
  expect(failure.stderr).toContain("evaluator-origin")
  expect(await readFile(resolve(root, "count"), "utf8")).toBe("once")
  await expect(readFile(resolve(root, "output/config/config-evaluation.mjs.map"))).rejects.toThrow()
})


test.each(["helmr.config.ts", "config-source.ts"])("program rejects %s when the root config entry is a symlink", async (imported) => {
  const root = await fixture()
  await put(root, "config-source.ts", 'throw Error("CONFIG EXECUTED");export default {}')
  await symlink("config-source.ts", resolve(root, "helmr.config.ts"))
  await put(root, "tasks/task.ts", `import config from "../${imported}";export const value=config;`)
  await expect(compileProgram({ root, outputRoot: resolve(root, "output"), architecture: "x86_64", nodeVersion: "24.19.0", config: { dirs: ["tasks"], ignorePatterns: [], compilePackages: [] } })).rejects.toThrow("build-only")
})

test("program does not suppress root config resolution errors other than absence", async () => {
  const root = await fixture()
  await symlink("helmr.config.ts", resolve(root, "helmr.config.ts"))
  await put(root, "tasks/task.ts", "export const value=1")
  await expect(compileProgram({ root, outputRoot: resolve(root, "output"), architecture: "x86_64", nodeVersion: "24.19.0", config: { dirs: ["tasks"], ignorePatterns: [], compilePackages: [] } })).rejects.toThrow("ELOOP")
})
