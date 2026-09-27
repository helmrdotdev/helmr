import { task, tokens, logger } from "@helmr/sdk"
import { randomUUID } from "node:crypto"
import { readFile, readlink, symlink, writeFile } from "node:fs/promises"
import { z } from "zod"

export const persistenceTask = task({
  id: "verification-persistence",
  maxDuration: "10m",
  retry: { enabled: false },
  payload: z.object({ marker: z.string().min(1), tokenId: z.string().min(1) }).strict(),
  run: async ({ marker, tokenId }, ctx) => {
    const nonce = randomUUID()
    const file = `/root/verification-${nonce}.json`
    await writeFile(file, JSON.stringify({ marker, nonce }), { flag: "wx" })
    await symlink(file, `${file}.link`)
    await logger.info("verification persistence waiting", { marker, nonce, runId: ctx.run.id })
    // Driver waits for a ready checkpoint AND a reclaimed old VM before resume.
    await tokens.ref(tokenId).wait({ timeout: "8m", idleTimeout: "2s" }).unwrap()
    if (await readlink(`${file}.link`) !== file) throw new Error("persisted symlink changed")
    const restored = JSON.parse(await readFile(`${file}.link`, "utf8"))
    if (restored.marker !== marker || restored.nonce !== nonce) throw new Error("persisted state changed")
    return { marker, nonce, runId: ctx.run.id, workspaceId: ctx.workspace.id }
  },
})
