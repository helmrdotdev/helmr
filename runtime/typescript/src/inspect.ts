import { resolve } from "node:path"
import { loadConfig } from "./config"

function cwdFrom(argv: readonly string[]): string {
  const index = argv.indexOf("--cwd")
  if (index < 0 || index + 1 >= argv.length) {
    throw new Error("--cwd is required")
  }
  return resolve(argv[index + 1] as string)
}

async function main(): Promise<void> {
  const config = await loadConfig(cwdFrom(process.argv.slice(2)))
  process.stdout.write(`${JSON.stringify({ project: config.project })}\n`)
}

await main()
