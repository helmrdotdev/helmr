import { mkdir, readFile, rename, writeFile } from "node:fs/promises"
import { join } from "node:path"
import { z } from "zod"

// Application-owned state, captured with the Workspace. Keep this directory out
// of commits and preserve it when preparing the next Run's repository checkout.
export async function conversation(cwd: string, sessionId: string, provider: "codex" | "claude") {
  const directory = join(cwd, ".helmr", "issue-fixer", encodeURIComponent(sessionId), provider)
  await mkdir(directory, { recursive: true, mode: 0o700 })
  const file = join(directory, "conversation.json")
  let id: string | undefined
  try {
    id = z.object({ id: z.string().min(1) }).parse(JSON.parse(await readFile(file, "utf8"))).id
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error
  }
  return {
    id,
    directory,
    async remember(value: string) {
      if (id === value) return
      if (id !== undefined) throw new Error("Native conversation changed unexpectedly")
      await writeFile(`${file}.tmp`, JSON.stringify({ id: value }), { mode: 0o600 })
      await rename(`${file}.tmp`, file)
      id = value
    },
  }
}
