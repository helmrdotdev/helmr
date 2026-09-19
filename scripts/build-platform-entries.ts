import { build } from "esbuild"
import { readFile, writeFile } from "node:fs/promises"

import { nodeVersion } from "./node-version.mjs"

const nodeTarget = `node${nodeVersion}`
if (process.argv[2] === "--node-target") {
  console.log(nodeTarget)
  process.exit(0)
}
const group = process.argv[2]
const check = process.argv.includes("--check")
// The host config evaluator runs on the developer's or CI's own Node, so it
// targets the supported host floor and carries its loader (jiti) inside one
// file the CLI embeds; nothing needs to be installed beside the project.
const hostNodeTarget = "node22"
const entries = group === "compiler" ? [
  ["compiler/typescript/src/program-compiler.ts", "internal/compiler/program-compiler.mjs"],
] : group === "hostconfig" ? [
  ["compiler/typescript/src/config-evaluator.ts", "internal/hostconfig/config-evaluator.mjs"],
] : group === "runtime" ? [
  ["runtime/typescript/src/entry.ts", "internal/runtime/entry.mjs"],
  ["runtime/typescript/src/module-preload.ts", "internal/runtime/module-preload.mjs"],
] : undefined
if (entries === undefined) throw new Error("expected compiler, hostconfig or runtime entry group")
for (const [entry, target] of entries) {
  const result = await build({
    entryPoints: [entry!], bundle: true, platform: "node", format: "esm", write: false,
    target: group === "hostconfig" ? hostNodeTarget : nodeTarget,
    // jiti's bundled CommonJS pieces call require(); an ES module has none.
    banner: group === "hostconfig" ? { js: 'import { createRequire as helmrCreateRequire } from "node:module"; const require = helmrCreateRequire(import.meta.url);' } : {},
    plugins: [{ name: "shared-language", setup(build) {
      build.onResolve({ filter: /^@helmr\/module-execution$/ }, () => ({ path: "../moduleexecution/loader.mjs", external: true }))
    } }],
  })
  const bytes = result.outputFiles[0]!.text
  if (check) {
    if (await readFile(target!, "utf8") !== bytes) throw new Error(`${target} is stale; regenerate ${group} entries`)
  } else await writeFile(target!, bytes)
}
