import { task } from "@helmr/sdk"

import { distrolessSandbox } from "../shared/sandboxes"
import { assert } from "./_assert"

export const distrolessStarts = task({
  id: "distroless-starts",
  sandbox: distrolessSandbox,
  maxDuration: 600,
  run: async (_payload, ctx) => {
    assert(process.cwd() === "/workspace", `expected /workspace cwd, got ${process.cwd()}`)
    return { runId: ctx.run.id, rootfs: "distroless" }
  },
})
