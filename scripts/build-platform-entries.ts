import { build } from "esbuild"
import { readFile, writeFile } from "node:fs/promises"

const group = process.argv[2]
const check = process.argv.includes("--check")
const entries = group === "compiler" ? [
  ["compiler/typescript/src/config-evaluator.ts", "internal/compiler/config-evaluator.mjs"],
  ["compiler/typescript/src/program-compiler.ts", "internal/compiler/program-compiler.mjs"],
] : group === "runtime" ? [
  ["runtime/typescript/src/entry.ts", "internal/runtime/entry.mjs"],
  ["runtime/typescript/src/module-preload.ts", "internal/runtime/module-preload.mjs"],
] : undefined
if (entries === undefined) throw new Error("expected compiler or runtime entry group")
for (const [entry, target] of entries) {
  const result = await build({
    entryPoints: [entry!], bundle: true, platform: "node", format: "esm", target: "node24.21", write: false,
    plugins: [{ name: "shared-language", setup(build) {
      build.onResolve({ filter: /^@helmr\/module-execution$/ }, () => ({ path: "../moduleexecution/loader.mjs", external: true }))
    } }],
  })
  const bytes = result.outputFiles[0]!.text
  if (check) {
    if (await readFile(target!, "utf8") !== bytes) throw new Error(`${target} is stale; regenerate ${group} entries`)
  } else await writeFile(target!, bytes)
}
