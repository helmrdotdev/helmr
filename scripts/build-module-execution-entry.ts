import { build } from "esbuild"
import { mkdir, readFile, writeFile } from "node:fs/promises"
import { dirname, resolve } from "node:path"

const check = process.argv.includes("--check")
const target = resolve("internal/moduleexecution/loader.mjs")
const result = await build({
  entryPoints: ["packages/module-execution/src/index.ts"],
  bundle: true,
  platform: "node",
  format: "esm",
  write: false,
  plugins: [{ name: "platform-typescript", setup(build) {
    build.onResolve({ filter: /^typescript$/ }, () => ({ path: "./typescript.cjs", external: true }))
  } }],
})
const bytes = result.outputFiles[0]!.text
await mkdir(dirname(target), { recursive: true })
if (check) {
  if (await readFile(target, "utf8") !== bytes) throw new Error("module execution entry is stale; run scripts/build-module-execution-entry.sh")
} else await writeFile(target, bytes)
// Local execution of embedded platform entries needs the same external API as
// the Nix Runtime/compiler closures. This dependency is not vendored in Git.
const library = resolve("packages/module-execution/node_modules/typescript/lib/typescript.js")
await writeFile(resolve(dirname(target), "typescript.cjs"), await readFile(library))
