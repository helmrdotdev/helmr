import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { dirname, join } from "node:path"
import { fileURLToPath } from "node:url"
import { run } from "./run.ts"

const python = "/opt/agentic-python/bin/python"
const script = join(dirname(fileURLToPath(import.meta.url)), "analysis.py")

// A data step: hand known input to Python, let NumPy's native code compute, and
// read the structured artifact back.
export async function pythonWork() {
  const directory = await mkdtemp(join(tmpdir(), "agentic-python-"))
  try {
    const input = join(directory, "input.json")
    const output = join(directory, "result.json")
    await writeFile(input, JSON.stringify({
      samples: [1, 2, 3, 4, 5, 6, 7, 8, 9, 10],
      matrix: [[3, 1], [1, 2]],
      vector: [9, 8],
    }))
    const finished = await run(python, [script, input, output])
    if (finished.code !== 0) {
      throw new Error(`python exited ${finished.code}: ${finished.stderr}`)
    }
    return { progress: finished.stdout.trim().split("\n"), result: JSON.parse(await readFile(output, "utf8")) }
  } finally {
    await rm(directory, { recursive: true, force: true })
  }
}
