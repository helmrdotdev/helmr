// Run after preparing the SDK/compiler entries and installing both examples.
// Exercises the ordinary bundle/compiler with locally installed dependencies;
// does not build a Linux image or qualify a deployed Computer.
import assert from "node:assert/strict"
import { cp, mkdtemp, readFile, rm, rename, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { resolve } from "node:path"
import { bundleProgram } from "../compiler/typescript/src/bundle"
import { nodeVersion, runEntry, runHostConfig } from "../compiler/typescript/src/test-process"

const repository = resolve(import.meta.dir, "..")
for (const kind of ["opencode", "pi"]) {
  const root = resolve(repository, "examples", `${kind}-assistant`)
  const starter = resolve(repository, "starters", kind)
  const shared = kind === "opencode" ? ["opencode-bridge.ts", "content.ts", "recovery.ts"] : ["pi-bridge.ts", "content.ts", "questions.ts"]
  for (const file of shared) {
    assert.equal(await readFile(resolve(root, "agents", file), "utf8"), await readFile(resolve(starter, "agents", file), "utf8"), `${kind}/${file} must match the starter`)
  }
  // OpenCode's questions.ts deliberately keeps one-time permission choices;
  // its application test verifies that a persistent grant is not offered.
  const examplePackage = JSON.parse(await readFile(resolve(root, "package.json"), "utf8"))
  const starterPackage = JSON.parse(await readFile(resolve(starter, "package.json"), "utf8"))
  for (const [name, version] of Object.entries(starterPackage.dependencies)) {
    assert.equal(examplePackage.dependencies[name], version, `${kind}: dependency ${name} must match the starter`)
  }
  const work = await mkdtemp(resolve(tmpdir(), "helmr-example-project-"))
  try {
    // Materialize local SDK file links as the captured project does. The
    // example may not bundle files from another checkout directory.
    const project = resolve(work, "project")
    for (const name of ["agents", "helmr.config.ts", "tsconfig.json", "package.json", "node_modules", ...(kind === "pi" ? ["data"] : [])]) {
      await cp(resolve(root, name), resolve(project, name), { recursive: true, dereference: true })
    }
    const config = runHostConfig(project).discovery
    const bundle = resolve(work, "bundle")
    await bundleProgram({ root: project, output: bundle, config, nodeVersion })
    const install = JSON.parse(await readFile(resolve(bundle, "install/package.json"), "utf8"))
    for (const name of config.external) assert.equal(install.dependencies[name], examplePackage.dependencies[name])
    assert.deepEqual(install.overrides, examplePackage.overrides, "deployment installation preserves dependency overrides")
    const payload = resolve(bundle, "payload")
    // Only the local analysis borrows these installed packages. Deployment
    // separately installs the emitted exact-version runtime manifest on Linux.
    await rename(resolve(project, "node_modules"), resolve(payload, "node_modules"))
    const configPath = resolve(work, "config.json")
    await writeFile(configPath, JSON.stringify(config))
    const result = runEntry("program-compiler", ["--analyze", payload, configPath, nodeVersion, `sha256:${"1".repeat(64)}`, resolve(bundle, "bundle.json"), work])
    assert.equal(result.outcome, "succeeded", JSON.stringify(result.error))
    const index = JSON.parse(result.files.find((file: { path: string }) => file.path === "helmr/definition-index.json").content)
    assert.deepEqual(index.agents.map((entry: { id: string }) => entry.id), [`${kind}-assistant`])
    assert.equal(index.templates.length, 1)
    if (kind === "pi") {
      const plan = JSON.parse(result.files.find((file: { path: string }) => file.path === "helmr/build-plan.json").content)
      const steps = plan.definitions.filter((entry: { kind: string }) => entry.kind === "template").flatMap((entry: any) => entry.manifest.imageBuild.images.flatMap((image: any) => image.steps))
      assert.ok(steps.some((step: any) => step.copySourceDir?.path === "data" && step.copySourceDir?.dst === "/workspace/data"), "the image includes the sample data")
      assert.match(await readFile(resolve(root, "data/sales.csv"), "utf8"), /^month,product,region,revenue_usd\n/)
    }
    console.log(`check example ${kind}: starter parity, dependencies, bundle and definitions passed`)
  } finally { await rm(work, { recursive: true, force: true }) }
}
