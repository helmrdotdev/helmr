import { mkdir, readFile, rename, writeFile } from "node:fs/promises"
import { join } from "node:path"
import { z } from "zod"

// Application-owned native history identity, captured with the Computer.
export async function conversation(cwd: string, sessionId: string, provider: "codex" | "claude") {
  const directory = join(cwd, ".helmr", "issue-fixer", encodeURIComponent(sessionId), provider)
  await mkdir(directory, { recursive: true, mode: 0o700 })
  const file = join(directory, "conversation.json")
  let id: string | undefined, established = false
  try {
    const record = z.object({ id: z.string().min(1), established: z.boolean() }).parse(JSON.parse(await readFile(file, "utf8")))
    id = record.id
    established = record.established
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error
  }
  async function persist(value: string, ready: boolean) {
    if (id !== undefined && id !== value) throw new Error("Native conversation changed unexpectedly")
    if (id === value && established === ready) return
    await writeFile(`${file}.tmp`, JSON.stringify({ id: value, established: ready }), { mode: 0o600 })
    await rename(`${file}.tmp`, file)
    id = value
    established = ready
  }
  return {
    id: established ? id : undefined,
    candidateId: id,
    directory,
    // A reserved UUID is not evidence that a native transcript exists yet.
    reserve: (value: string) => persist(value, established),
    remember: (value: string) => persist(value, true),
  }
}
