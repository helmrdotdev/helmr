// Executed by the real native tool, inside its session directory. Assets are
// immutable image inputs; all mutations remain in this session's workload copy.
import assert from "node:assert/strict"
import { createHash } from "node:crypto"
import { spawnSync } from "node:child_process"
import { cp, mkdir, readFile, readdir, lstat, rm, writeFile } from "node:fs/promises"
import { createRequire } from "node:module"
import { join, resolve } from "node:path"
import { pathToFileURL } from "node:url"

const [kind, sequenceText, assetsArgument = "/opt/native-performance"] = process.argv.slice(2)
assert(["no-op", "edit-test", "dependencies"].includes(kind))
const sequence = Number(sequenceText)
assert(Number.isSafeInteger(sequence) && sequence > 0)
const assets = resolve(assetsArgument)
const root = resolve("performance-workload")
await mkdir(root, { recursive: true })
const hash = value => createHash("sha256").update(value).digest("hex")
function run(command, args, cwd) {
  // These commands take file arguments and never read standard input.
  const result = spawnSync(command, args, { cwd, stdio: ["ignore", "pipe", "pipe"], encoding: "utf8", maxBuffer: 4 * 1024 * 1024 })
  assert.ifError(result.error)
  assert.equal(result.status, 0, `${command}: ${result.stderr}\n${result.stdout}`)
  return result.stdout
}
async function footprint(path) {
  let files = 0, logicalBytes = 0, allocatedBytes = 0
  async function visit(path) {
    const entry = await lstat(path)
    allocatedBytes += entry.blocks * 512
    if (entry.isDirectory()) {
      for (const name of await readdir(path)) await visit(join(path, name))
    } else { files++; logicalBytes += entry.size }
  }
  await visit(path)
  return { files, logicalBytes, allocatedBytes }
}
const started = performance.now()
let detail
if (kind === "no-op") {
  // No repository or dependency writes. Native conversation persistence still
  // occurs, so this is not a claim of a zero-dirty Computer.
  detail = { sourceIdentity: JSON.parse(await readFile(join(assets, "source.json"), "utf8")) }
} else if (kind === "edit-test") {
  const repository = join(root, "repository")
  if (sequence === 1) {
    await mkdir(repository)
    run("tar", ["-xf", join(assets, "repository.tar"), "-C", repository], root)
  }
  const file = join(repository, "sdk/typescript/src/content.ts")
  const before = await readFile(file, "utf8")
  const limit = sequence % 2 ? 63 : 62
  const after = before.replace(/export const CONTENT_PARTS = \d+/, `export const CONTENT_PARTS = ${limit}`)
  assert.notEqual(before, after)
  await writeFile(file, after)
  const { build } = createRequire(join(assets, "tools/package.json"))("esbuild")
  const output = join(root, `content-${sequence}.mjs`)
  await build({ entryPoints: [file], bundle: true, platform: "node", format: "esm", outfile: output, tsconfigRaw: {} })
  const { normalizeContent, CONTENT_PARTS } = await import(pathToFileURL(output).href)
  assert.equal(CONTENT_PARTS, limit)
  const parts = Array.from({ length: limit }, () => ({ type: "text", text: "boundary" }))
  assert.deepEqual(normalizeContent(parts), parts)
  assert.throws(() => normalizeContent([...parts, parts[0]]), error => error.code === "content_limit_exceeded")
  assert.throws(() => normalizeContent([{ type: "json", value: NaN }]), error => error.code === "invalid_arguments")
  assert.deepEqual(normalizeContent("hello"), [{ type: "text", text: "hello" }])
  detail = { beforeSha256: hash(before), afterSha256: hash(after), bundleSha256: hash(await readFile(output)), assertions: 5, limit }
} else {
  const dependencies = join(root, "dependencies")
  await mkdir(dependencies, { recursive: true })
  for (const file of ["package.json", "package-lock.json"]) await cp(join(assets, "dependencies", file), join(dependencies, file))
  await rm(join(dependencies, "node_modules"), { recursive: true, force: true })
  const output = run("npm", ["ci", "--offline", "--ignore-scripts", "--no-audit", "--no-fund", "--update-notifier=false", "--logs-dir", join(root, "npm-logs"), "--cache", join(assets, "npm-cache")], dependencies)
  for (const name of ["@openai/codex", "@anthropic-ai/claude-agent-sdk"]) {
    const installed = JSON.parse(await readFile(join(dependencies, "node_modules", name, "package.json"), "utf8"))
    assert.equal(installed.name, name)
  }
  detail = { lockSha256: hash(await readFile(join(dependencies, "package-lock.json"))), npmOutput: output.trim(), tarballCache: "warm", installDirectory: "fresh" }
}
const operationMs = performance.now() - started
const inventoryStarted = performance.now()
const inventory = await footprint(root)
const result = { kind, sequence, operationMs, inventoryMs: performance.now() - inventoryStarted, inventory, detail }
await writeFile(resolve(`performance-result-${sequence}.json`), JSON.stringify(result))
console.log(JSON.stringify(result))
