// Host config evaluator, embedded in the Helmr CLI. It runs once per build in
// a fresh process on the invoking machine, before any Linux environment
// exists: the config's ordinary imports resolve against the project's host
// installation. It is trusted local code, like a package script, not a sandbox.
import { canonicalizeJsonValue, isBuilder, type BuilderStep, type JsonValue } from "@helmr/sdk/internal"
import { createJiti } from "jiti/static"
import { createWriteStream } from "node:fs"
import { resolve } from "node:path"
import { pathToFileURL } from "node:url"
import { loadConfig } from "./config"

const maxDocumentBytes = 1 << 20

// Steps come from whichever @helmr/sdk copy the project installed. Re-read them
// as plain data so a newer or older SDK cannot hand the CLI a shape it would
// misinterpret.
function builderSteps(value: unknown): JsonValue[] {
  if (!isBuilder(value) || !Array.isArray(value.steps)) {
    throw new Error("config build.builder must be created by builder()")
  }
  return value.steps.map((step: BuilderStep, index: number): JsonValue => {
    const keys = Object.keys(step).sort().join(",")
    if (step.kind === "run" && keys === "argv,kind" && Array.isArray(step.argv) && step.argv.every((argument) => typeof argument === "string")) {
      return { kind: "run", argv: [...step.argv] }
    }
    if (step.kind === "copy" && keys === "destination,kind,source" && typeof step.source === "string" && typeof step.destination === "string") {
      return { kind: "copy", source: step.source, destination: step.destination }
    }
    throw new Error(`builder step ${index + 1} from the installed @helmr/sdk is not understood by this Helmr CLI; align their versions`)
  })
}

async function main(): Promise<void> {
  if (process.argv.length !== 3 || process.argv[2] === undefined) {
    throw new Error("Config Evaluator requires the project root")
  }
  const root = resolve(process.argv[2])
  const path = resolve(root, "helmr.config.ts")
  const jiti = createJiti(pathToFileURL(path).href, { fsCache: false, moduleCache: false, tsconfigPaths: true })
  // jiti answers a missing default export with the module itself; only an own
  // default export is the config.
  const config = await loadConfig(path, async (url) => {
    const namespace = await jiti.import(url.href) as Record<string, unknown>
    return { default: Object.hasOwn(namespace, "default") ? namespace["default"] : undefined }
  })
  const build: Record<string, JsonValue> = {
    builder: { steps: builderSteps(config.build.builder) },
    secrets: [...config.build.secrets],
  }
  if (config.build.installCommand !== undefined) build["installCommand"] = config.build.installCommand
  const body = canonicalizeJsonValue({
    discovery: { dirs: [...config.dirs], ignorePatterns: [...config.ignorePatterns] },
    build,
  })
  if (body.byteLength === 0 || body.byteLength > maxDocumentBytes) {
    throw new Error("resolved config size is invalid")
  }
  const frame = new Uint8Array(4 + body.byteLength)
  new DataView(frame.buffer).setUint32(0, body.byteLength, false)
  frame.set(body, 4)
  const output = createWriteStream("", { fd: 3, autoClose: false })
  await new Promise<void>((done, reject) => {
    output.once("error", reject)
    output.end(frame, done)
  })
}

// Report the failure and its causes (the config's own error keeps its stack)
// without the embedded evaluator's frames.
try {
  await main()
} catch (error) {
  const lines: string[] = []
  for (let current: unknown = error; current !== undefined && current !== null; current = (current as { cause?: unknown }).cause) {
    const text = current instanceof Error ? (current === error ? current.message : current.stack ?? current.message) : String(current)
    lines.push(text)
  }
  console.error(lines.join("\ncaused by: "))
  process.exitCode = 1
}
