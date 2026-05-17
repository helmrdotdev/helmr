import { task } from "@helmr/sdk"

import { defaultRootSandbox } from "../shared/sandboxes"
import { assert, command } from "./_assert"

export const defaultRoot = task({
  id: "default-root",
  sandbox: defaultRootSandbox,
  maxDuration: 600,
  run: async (_payload, ctx) => {
    const username = (await command(["id", "-un"])).trim()
    const uid = (await command(["id", "-u"])).trim()
    assert(username === "root", `expected root user, got ${username}`)
    assert(uid === "0", `expected uid 0, got ${uid}`)
    return { runId: ctx.run.id, username, uid }
  },
})
