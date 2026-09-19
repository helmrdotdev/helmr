import assert from "node:assert/strict"
import { mkdtempSync, mkdirSync, writeFileSync, rmSync, symlinkSync, realpathSync } from "node:fs"
import { tmpdir } from "node:os"
import { join, dirname } from "node:path"
import { spawnSync } from "node:child_process"
import { test } from "node:test"

const adapter = new URL("../../../internal/moduleexecution/loader.mjs", import.meta.url).pathname
function fixture(files: Record<string, string | object>, body: string, prefix = "helmr-module-execution-") {
  const root = realpathSync(mkdtempSync(join(tmpdir(), prefix)))
  for (const [path, value] of Object.entries({ "package.json": { type: "module" }, ...files })) {
    mkdirSync(dirname(join(root, path)), { recursive: true })
    writeFileSync(join(root, path), typeof value === "string" ? value : JSON.stringify(value))
  }
  const runner = join(root, "runner.mjs")
  writeFileSync(runner, `import {installModuleExecution} from ${JSON.stringify(adapter)};const execution=installModuleExecution({root:${JSON.stringify(root)}});\n${body}`)
  return { root, run: () => spawnSync(process.execPath, ["--no-strip-types", "--no-global-search-paths", "--enable-source-maps", runner], { encoding: "utf8", env: { PATH: process.env["PATH"] } }), close: () => rmSync(root, { recursive: true, force: true }) }
}
function check(files: Record<string, string | object>, body: string, expected: unknown) {
  const f = fixture(files, body)
  try { const result = f.run(); assert.equal(result.status, 0, result.stderr); assert.deepEqual(JSON.parse(result.stdout), expected) } finally { f.close() }
}

test("mixed installed JS, dynamic TS, assets and native module identity", () => {
  check({
    "node_modules/p/package.json": { type: "module", exports: "./entry.js" },
    "node_modules/p/entry.js": 'export {state} from "./value.ts";export const dynamic=()=>import("./value.ts")',
    "node_modules/p/value.ts": 'import{readFileSync}from"node:fs";globalThis.count=(globalThis.count??0)+1;export const state:object={count:globalThis.count,asset:readFileSync(new URL("./data.txt",import.meta.url),"utf8")}',
    "node_modules/p/data.txt": "installed",
    "main.ts": 'import{state,dynamic}from"p";export default [state,state===(await dynamic()).state]',
  }, 'console.log(JSON.stringify((await execution.importSourceExports(new URL("./main.ts",import.meta.url))).default))', [{ count: 1, asset: "installed" }, true])
})

test("nearest JSONC extends, aliases and class field lowering agree", () => {
  check({
    "tsconfig.json": { compilerOptions: { paths: { "@value": ["./wrong.ts"] } } },
    "wrong.ts": 'export default "root"',
    "sub/tsconfig.json": '{/* nearest */"extends":"./base.json"}',
    "sub/base.json": { compilerOptions: { paths: { "@value": ["./value.ts"] }, target: "es2018", jsxFactory: "h", jsx: "preserve" } },
    "sub/value.ts": 'export default "nested"',
    "sub/main.tsx": 'import value from"@value";const h=()=>"jsx";let effect="own";class B{set x(v:number){effect="setter"}}class C extends B{x=1}new C;export default [value,<div/>,effect]',
  }, 'console.log(JSON.stringify((await execution.importSourceExports(new URL("./sub/main.tsx",import.meta.url))).default))', ["nested", "jsx", "setter"])
})

test("native conditions and multiple installed instances", () => {
  check({
    "node_modules/p/package.json": { exports: { import: "./import.mts", require: "./require.cts" } },
    "node_modules/p/import.mts": 'export default "import"',
    "node_modules/p/require.cts": 'module.exports="require"',
    "main.ts": 'import value from"p";import{createRequire}from"node:module";export default [value,createRequire(import.meta.url)("p")]',
  }, 'console.log(JSON.stringify((await execution.importSourceExports(new URL("./main.ts",import.meta.url))).default))', ["import", "require"])
})

test("genuine ESM values are not CJS-unwrapped; CJS uses known format", () => {
  check({ "value.ts": 'export default {__esModule:true,default:{dirs:["wrong"]},dirs:["right"]}' }, 'console.log(JSON.stringify((await execution.importSourceExports(new URL("./value.ts",import.meta.url))).default))', { __esModule: true, default: { dirs: ["wrong"] }, dirs: ["right"] })
  check({ "package.json": {}, "value.ts": 'globalThis.count=(globalThis.count??0)+1;const config={dirs:["tasks"]};export default config' }, 'const value=await execution.importSourceExports(new URL("./value.ts",import.meta.url));console.log(JSON.stringify([value.default,globalThis.count]))', [{ dirs: ["tasks"] }, 1])
})

test("root config aliases, symlinks and queries are build-only", () => {
  const f = fixture({
    "helmr.config.ts": 'throw Error("config executed")',
    "tsconfig.json": { compilerOptions: { paths: { config: ["./alias.ts"] } } },
    "main.ts": 'import "config"',
  }, 'await import("./main.ts")')
  try {
    symlinkSync("helmr.config.ts", join(f.root, "alias.ts"))
    const result = f.run(); assert.notEqual(result.status, 0); assert.match(result.stderr, /build-only/); assert.doesNotMatch(result.stderr, /Error: config executed/)
  } finally { f.close() }
  const q = fixture({ "helmr.config.ts": "export default {}" }, 'await import("./helmr.config.ts?again")')
  try { assert.match(q.run().stderr, /build-only/) } finally { q.close() }
})

test("config escapes and cycles fail before outside content is consumed", () => {
  for (const tsconfig of [{ extends: "../outside.json" }, { extends: "./tsconfig.json" }]) {
    const f = fixture({ "tsconfig.json": tsconfig, "main.ts": "export default 1" }, 'await import("./main.ts")')
    try { const result = f.run(); assert.notEqual(result.status, 0); assert.match(result.stderr, /Cannot read file|Circularity/) } finally { f.close() }
  }
  const f = fixture({ "main.ts": "export default 1" }, 'await import("./main.ts")')
  try { symlinkSync("/etc/passwd", join(f.root, "tsconfig.json")); assert.match(f.run().stderr, /must stay inside Program/) } finally { f.close() }
})

test("alias fallback preserves broken package targets and native JS wins collisions", () => {
  check({ "helper.js": 'export default "JS"', "helper.ts": 'export default "TS"', "main.ts": 'export{default}from"./helper"' }, 'console.log(JSON.stringify((await import("./main.ts")).default))', "JS")
  const f = fixture({
    "tsconfig.json": { compilerOptions: { paths: { p: ["./broken", "./good.ts"] } } },
    "broken/package.json": { main: "missing.js" }, "good.ts": 'export default "hidden"', "main.ts": 'import value from"p";export default value',
  }, 'await import("./main.ts")')
  try { const result=f.run(); assert.notEqual(result.status,0); assert.match(result.stderr,/broken|directory|Directory/) } finally { f.close() }
})

test("customer imports cannot enter trusted platform bootstrap", () => {
  const f = fixture({ "main.ts": `import ${JSON.stringify(adapter)}` }, 'await import("./main.ts")')
  try { assert.match(f.run().stderr, /must stay inside Program/) } finally { f.close() }
})

test("dirty ancestor lookup locations and non-object root manifest reject entry", () => {
  const f = fixture({ "package.json": [] }, 'throw Error("user ran")')
  try { assert.match(f.run().stderr, /must contain a JSON object/) } finally { f.close() }
  const parent = fixture({}, '')
  try {
    mkdirSync(join(parent.root,"node_modules"));mkdirSync(join(parent.root,"project"));writeFileSync(join(parent.root,"project/package.json"),'{}')
    writeFileSync(join(parent.root,"runner.mjs"),`import{installModuleExecution}from${JSON.stringify(adapter)};installModuleExecution({root:${JSON.stringify(join(parent.root,"project"))}})`)
    assert.match(parent.run().stderr,/ancestor node_modules search location/)
  } finally { parent.close() }
})


test("typeless Node JS syntax detection preserves genuine namespace", () => {
  check({"package.json": {}, "main.js": 'export const example=42;export default {value:7,__esModule:true,default:13}'}, 'console.log(JSON.stringify(await execution.importSourceExports(new URL("./main.js",import.meta.url))))', {example:42,default:{value:7,__esModule:true,default:13}})
})
test("data modules do not inherit trusted bootstrap authority", () => {
 const f=fixture({}, 'await import("./main.ts")')
 try {
  const platform=join(f.root,"../platform-"+f.root.split("/").pop());mkdirSync(join(platform,"helmr"),{recursive:true});writeFileSync(join(platform,"helmr/entry.mjs"),'export const value=42')
  try {
   writeFileSync(join(f.root,"main.ts"),`import ${JSON.stringify('data:text/javascript,'+encodeURIComponent('import '+JSON.stringify("file://"+join(platform,"helmr/entry.mjs"))))}`)
   writeFileSync(join(f.root,"runner.mjs"),`import{installModuleExecution}from${JSON.stringify(adapter)};installModuleExecution({root:${JSON.stringify(f.root)},platformRoot:${JSON.stringify(platform)}});await import('./main.ts')`)
   assert.match(f.run().stderr,/must stay inside Program/)
  } finally {rmSync(platform,{recursive:true,force:true})}
 } finally {f.close()}
})

test("scoped aliases, nested versions, package self-reference and imports stay native", () => {
  check({
    "package.json": { name: "project", type: "module", exports: "./self.ts", imports: { "#value": "./self.ts" } },
    "self.ts": 'export default "self"',
    "node_modules/@scope/alias/package.json": { name: "original-name", type: "module", exports: "./entry.ts" },
    "node_modules/@scope/alias/entry.ts": 'import nested from"duplicate";export default nested',
    "node_modules/@scope/alias/node_modules/duplicate/package.json": { type: "module", exports: "./index.ts" },
    "node_modules/@scope/alias/node_modules/duplicate/index.ts": 'export default "nested-v2"',
    "node_modules/duplicate/package.json": { type: "module", exports: "./index.ts" },
    "node_modules/duplicate/index.ts": 'export default "hoisted-v1"',
    "main.ts": 'import a from"@scope/alias";import b from"duplicate";import c from"#value";import d from"project";export default [a,b,c,d]',
  }, 'console.log(JSON.stringify((await import("./main.ts")).default))', ["nested-v2", "hoisted-v1", "self", "self"])
})

test("CJS cache and true ESM module.exports export do not confuse extraction", () => {
  check({ "package.json": {}, "value.js": 'module.exports={value:42}', "main.cts": 'const a=require("./value.js");module.exports={a,same:a===require("./value.js")}' }, 'const v=await execution.importSourceExports(new URL("./main.cts",import.meta.url));console.log(JSON.stringify([v.default.a.value,v.default.same]))', [42,true])
  check({ "main.mjs": 'const value={__esModule:true,default:13};export{value as "module.exports"};export default value' }, 'const v=await execution.importSourceExports(new URL("./main.mjs",import.meta.url));console.log(JSON.stringify(v.default))', {__esModule:true,default:13})
})

test("source maps report original TS lines and thrown modules are not retried", () => {
  const f = fixture({ "main.ts": 'globalThis.count=(globalThis.count??0)+1;\nconst value:number=42;\nthrow new Error("once")' }, 'for(let i=0;i<2;i++){try{await import("./main.ts")}catch(e){if(i===0)console.log(e.stack)}}console.log("count="+globalThis.count)')
  try { const result=f.run();assert.equal(result.status,0,result.stderr);assert.match(result.stdout,/main.ts:3/);assert.match(result.stdout,/count=1/) } finally {f.close()}
})

test("malformed config and export denials remain actionable failures", () => {
  const cases: Record<string, string | object>[] = [
    {"tsconfig.json":'{"compilerOptions": { invalid }}',"main.ts":"export default 1"},
    {"node_modules/p/package.json":JSON.stringify({type:"module",exports:{".":"./index.ts"}}),"main.ts":'import "p/private"'},
  ]
  for(const files of cases) {
    const f=fixture(files,'await import("./main.ts")')
    try {const result=f.run();assert.notEqual(result.status,0);assert.match(result.stderr,/Property assignment expected|not defined by "exports"|not exported/)} finally {f.close()}
  }
})

test("JS callers share nearest aliases and TS relative/index fallback", () => {
  {
    for (const commonjs of [false, true]) {
      const entry = commonjs ? "entry.cjs" : "entry.js"
      check({
        "tsconfig.json": { compilerOptions: { paths: { "@value": ["./wrong.ts"] } } },
        "wrong.ts": 'throw new Error("root config selected for installed caller")',
        "node_modules/p/package.json": { type: commonjs ? "commonjs" : "module", exports: `./${entry}` },
        "node_modules/p/tsconfig.json": { extends: "./base.json" },
        "node_modules/p/base.json": { compilerOptions: { paths: { "@value": ["./value.ts"], fallback: ["./absent.ts"] }, jsxFactory: "h", jsx: "react" } },
        [`node_modules/p/${entry}`]: commonjs
          ? 'const value=require("@value").default,widget=require("./widget").default,index=require("./dir").default,js=require("./preferred.js"),fallback=require("fallback");module.exports=async()=>[value,widget,index,js,fallback,(await import("./value.js")).default.default]'
          : 'import value from"@value";import widget from"./widget";import index from"./dir";import js from"./preferred.js";import fallback from"fallback";export default async()=>[value,widget,index,js,fallback,(await import("./value.js")).default]',
        "node_modules/p/value.ts": 'export default "nearest"',
        "node_modules/p/widget.tsx": 'const h=()=>"jsx";export default <div/>',
        "node_modules/p/dir/index.ts": 'export default "index"',
        "node_modules/p/preferred.js": commonjs ? 'module.exports="native-js"' : 'export default "native-js"',
        "node_modules/p/preferred.ts": 'throw new Error("existing JS was replaced")',
        "node_modules/fallback/package.json": { exports: "./index.cjs" },
        "node_modules/fallback/index.cjs": 'module.exports="native-package"',
      }, 'console.log(JSON.stringify(await (await import("p")).default()))', ["nearest", "jsx", "index", "native-js", "native-package", "nearest"])
    }
  }
})

test("linked JS callers select config from canonical source ancestry", () => {
  const f = fixture({
    "tsconfig.json": { compilerOptions: { paths: { "@value": ["./wrong.ts"] } } },
    "wrong.ts": 'throw new Error("logical config ancestry selected")',
    "node_modules/placeholder": "",
    "packages/tsconfig.json": { compilerOptions: { paths: { "@value": ["./value.ts"] } } },
    "packages/value.ts": 'export default "canonical"',
    "packages/p/package.json": { type: "module", exports: "./entry.mjs" },
    "packages/p/entry.mjs": 'export {default} from "@value"',
  }, 'const value=(await import("p")).default;console.log(JSON.stringify([value,[...execution.configReads.keys()].map(p=>p.slice(execution.root.length+1))]))')
  try {
    symlinkSync("../packages/p", join(f.root, "node_modules/p"))
    const result = f.run()
    assert.equal(result.status, 0, result.stderr)
    assert.deepEqual(JSON.parse(result.stdout), ["canonical", ["tsconfig.json", "packages/tsconfig.json"]])
  } finally { f.close() }
})

test("JS resolution retains config, package, syntax and source-boundary errors", () => {
  const cases: { files: Record<string, string | object>; expected: RegExp }[] = [
    { files: { "tsconfig.json": '{"compilerOptions": { invalid }}', "main.js": 'import "p"' }, expected: /Property assignment expected/ },
    { files: { "tsconfig.json": { extends: "../outside.json" }, "main.js": 'import "p"' }, expected: /Cannot read file/ },
    { files: { "tsconfig.json": { compilerOptions: { paths: { p: ["./broken", "./good.ts"] } } }, "broken/package.json": { main: "missing.js" }, "good.ts": 'export default "hidden"', "main.js": 'import "p"' }, expected: /broken|directory|Directory/ },
    { files: { "node_modules/p/package.json": { exports: { ".": "./main.js" } }, "node_modules/p/hidden.ts": 'export default 1', "main.js": 'import "p/hidden"' }, expected: /not defined by "exports"|not exported/ },
    { files: { "node_modules/p/package.json": '{ invalid }', "main.js": 'import "p"' }, expected: /Invalid package config/ },
    { files: { "bad.js": 'export const =', "bad.ts": 'export default "hidden"', "main.js": 'import "./bad.js"' }, expected: /SyntaxError/ },
    { files: { "tsconfig.json": { compilerOptions: { paths: { config: ["./helmr.config.ts"] } } }, "helmr.config.ts": 'throw new Error("config replayed")', "main.js": 'import "config"' }, expected: /build-only/ },
    { files: { "tsconfig.json": { compilerOptions: { paths: { outside: ["../outside.ts"] } } }, "main.js": 'import "outside"' }, expected: /must stay inside Program/ },
  ]
  for (const { files, expected } of cases) {
    const f = fixture(files, 'await import("./main.js")')
    try {
      const result = f.run()
      assert.notEqual(result.status, 0)
      assert.match(result.stderr, expected)
      assert.doesNotMatch(result.stderr, /Error: config replayed/)
    } finally { f.close() }
  }
})

test("aliases preserve literal filename characters and nearest config in encoded roots", () => {
  for (const commonjs of [false, true]) {
    {
      const f = fixture({
        "package.json": { type: commonjs ? "commonjs" : "module" },
        "tsconfig.json": { compilerOptions: { paths: { "@value": ["./wrong.ts"] } } },
        "wrong.ts": 'throw Error("wrong config ancestry")',
        "nested#%?/tsconfig.json": { extends: "./base#%?.json" },
        "nested#%?/base#%?.json": { compilerOptions: { paths: { "@value": ["./value#%?.ts"], "@index": ["./dir#%?"] } } },
        "nested#%?/value#%?.ts": 'export default 42',
        "nested#%?/dir#%?/index.ts": 'export default 43',
        "nested#%?/main.js": commonjs
          ? 'module.exports=[require("@value").default,require("@index").default]'
          : 'import value from"@value";import index from"@index";export default [value,index]',
      }, 'const value=(await import("./nested%23%25%3F/main.js")).default;console.log(JSON.stringify([value,[...execution.configReads.keys()].map(p=>p.slice(execution.root.length+1))]))', "helmr-module-#%?-")
      try {
        const result = f.run()
        assert.equal(result.status, 0, result.stderr)
        assert.deepEqual(JSON.parse(result.stdout), [[42, 43], ["nested#%?/tsconfig.json", "nested#%?/base#%?.json"]])
      } finally { f.close() }
    }
  }
})

test("relative require preserves literal # percent and question-mark names", () => {
  const f = fixture({
    "package.json": {},
    "main.cjs": 'module.exports=[require("./value#name").default,require("./value%name.js").default,require("./value?name").default,require("./dir#%?").default,require("./native#%?.js")]',
    "value#name.ts": 'export default "hash"',
    "value%name.ts": 'export default "percent"',
    "value?name.ts": 'export default "question"',
    "dir#%?/index.ts": 'export default "index"',
    "native#%?.js": 'module.exports="native"',
    "native#%?.ts": 'throw Error("native success replaced")',
  }, 'console.log(JSON.stringify((await import("./main.cjs")).default))', "helmr-module-#%?-")
  try {
    const result = f.run()
    assert.equal(result.status, 0, result.stderr)
    assert.deepEqual(JSON.parse(result.stdout), ["hash", "percent", "question", "index", "native"])
  } finally { f.close() }
})

test("ESM encoded filenames retain query and fragment module identity on fallback", () => {
  check({
    "value#%?.ts": 'globalThis.loads=(globalThis.loads??0)+1;export const state={load:globalThis.loads,url:import.meta.url}',
    "main.js": 'const a=await import("./value%23%25%3F.js?one#first");const same=await import("./value%23%25%3F.js?one#first");const query=await import("./value%23%25%3F.js?two#first");const fragment=await import("./value%23%25%3F.js?one#second");export default [a===same,a!==query,a!==fragment,globalThis.loads,[a,query,fragment].map(v=>v.state.url.split("/").pop())]',
  }, 'console.log(JSON.stringify((await import("./main.js")).default))', [true, true, true, 3, ["value%23%25%3F.ts?one#first", "value%23%25%3F.ts?two#first", "value%23%25%3F.ts?one#second"]])
})

test("encoded paths retain native errors and canonical build-only config guards", () => {
  const cases: { files: Record<string, string | object>; expected: RegExp; setup?: (root: string) => void }[] = [
    { files: { "bad#%?.js": 'module.exports = ;', "bad#%?.ts": 'export default "hidden"', "main.cjs": 'require("./bad#%?.js")' }, expected: /SyntaxError/ },
    { files: { "tsconfig.json": { compilerOptions: { paths: { p: ["./broken#%?", "./good.ts"] } } }, "broken#%?/package.json": { main: "missing.js" }, "good.ts": 'export default "hidden"', "main.cjs": 'require("p")' }, expected: /broken.*|directory|Directory/ },
    { files: { "helmr.config.ts": 'throw Error("config replayed")', "tsconfig.json": { compilerOptions: { paths: { config: ["./config#%?.ts"] } } }, "main.cjs": 'require("config")' }, expected: /build-only/, setup: root => symlinkSync("helmr.config.ts", join(root, "config#%?.ts")) },
    { files: { "main.cjs": 'require("./escape#%?/hidden")' }, expected: /must stay inside Program/, setup: root => symlinkSync("..", join(root, "escape#%?")) },
  ]
  for (const { files, expected, setup } of cases) {
    const f = fixture(files, 'await import("./main.cjs")', "helmr-module-#%?-")
    try {
      setup?.(f.root)
      const result = f.run()
      assert.notEqual(result.status, 0)
      assert.match(result.stderr, expected)
      assert.doesNotMatch(result.stderr, /Error: config replayed/)
    } finally { f.close() }
  }
})
