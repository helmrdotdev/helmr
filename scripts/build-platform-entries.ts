import { build } from "esbuild"
import { randomUUID } from "node:crypto"
import { mkdir, readFile, rename, rm, writeFile } from "node:fs/promises"
import { dirname, resolve } from "node:path"

import { nodeVersion } from "./node-version.mjs"

const nodeTarget = `node${nodeVersion}`
if (process.argv[2] === "--node-target") {
  console.log(nodeTarget)
  process.exit(0)
}
const group = process.argv[2]
const outputIndex = process.argv.indexOf("--out-dir")
const outputDirectory = outputIndex === -1 ? "." : process.argv[outputIndex + 1]
if (!outputDirectory) throw new Error("--out-dir requires a directory")
// The host config evaluator runs on the developer's or CI's own Node, so it
// targets the supported host floor and carries its loader (jiti) inside one
// file the CLI embeds; nothing needs to be installed beside the project.
const hostNodeTarget = "node22"
const groups: Record<string, [string, string][]> = {
  compiler: [["compiler/typescript/src/program-compiler.ts", "internal/compiler/program-compiler.mjs"]],
  hostconfig: [["compiler/typescript/src/config-evaluator.ts", "internal/hostconfig/config-evaluator.mjs"]],
  runtime: [
    ["runtime/typescript/src/entry.ts", "internal/runtime/entry.mjs"],
    ["runtime/typescript/src/module-preload.ts", "internal/runtime/module-preload.mjs"],
  ],
}
const selected = group === "all" ? Object.entries(groups) : group && groups[group] ? [[group, groups[group]]] as const : undefined
if (selected === undefined) throw new Error("expected all, compiler, hostconfig or runtime entry group")
for (const [entryGroup, entries] of selected) for (const [entry, relativeTarget] of entries) {
  const target = resolve(outputDirectory, relativeTarget)
  const result = await build({
    entryPoints: [entry], bundle: true, platform: "node", format: "esm", write: false,
    target: entryGroup === "hostconfig" ? hostNodeTarget : nodeTarget,
    // jiti's bundled CommonJS pieces call require(); an ES module has none.
    banner: entryGroup !== "runtime" ? { js: 'import { createRequire as helmrCreateRequire } from "node:module"; const require = helmrCreateRequire(import.meta.url);' } : {},
    external: entryGroup === "compiler" ? ["esbuild"] : [],
  })
  const bytes = result.outputFiles[0]!.text
  const previous = await readFile(target, "utf8").catch((error: unknown) => {
    if (error instanceof Error && "code" in error && error.code === "ENOENT") return undefined
    throw error
  })
  if (previous !== bytes) {
    await mkdir(dirname(target), { recursive: true })
    const temporary = `${target}.${randomUUID()}.tmp`
    try {
      await writeFile(temporary, bytes)
      await rename(temporary, target)
    } finally {
      await rm(temporary, { force: true })
    }
  }
}
