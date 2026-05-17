import { task } from "@helmr/sdk"

import { defaultPathSandbox } from "../shared/sandboxes"
import { assert } from "./_assert"

export const defaultWorkdirPath = task({
  id: "default-workdir-path",
  sandbox: defaultPathSandbox,
  maxDuration: 600,
  run: async (_payload, ctx) => {
    assert(
      process.cwd() === "/workspace-default",
      `expected workspace cwd, got ${process.cwd()}`,
    )
    assert(
      process.env["PATH"] === "/usr/local/bin:/usr/bin:/bin",
      `expected default PATH, got ${process.env["PATH"] ?? "<missing>"}`,
    )
    return { runId: ctx.run.id, cwd: process.cwd(), path: process.env["PATH"] }
  },
})
