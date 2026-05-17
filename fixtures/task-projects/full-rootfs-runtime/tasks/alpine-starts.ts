import { task } from "@helmr/sdk"

import { alpineSandbox } from "../shared/sandboxes"
import { assert } from "./_assert"

export const alpineStarts = task({
  id: "alpine-starts",
  sandbox: alpineSandbox,
  maxDuration: 600,
  run: async (_payload, ctx) => {
    assert(process.cwd() === "/workspace", `expected /workspace cwd, got ${process.cwd()}`)
    return { runId: ctx.run.id, rootfs: "alpine" }
  },
})
