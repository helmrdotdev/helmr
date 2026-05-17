import { task } from "@helmr/sdk"

import { contractSandbox } from "../shared/sandboxes"
import { assertVisibleFile } from "./_assert"

export const failureBoundary = task({
  id: "failure-boundary",
  sandbox: contractSandbox,
  maxDuration: 900,
  run: async (_payload, ctx) => {
    console.log(`failure-run-id:${ctx.run.id}`)
    await assertVisibleFile("/tmp/task/failure-rootfs-write.txt", "rootfs failure\n")
    await assertVisibleFile("/tmp/failure-rootfs-tmp-write.txt", "tmp failure\n")
    await assertVisibleFile("/workspace/failure-workspace-write.txt", "workspace failure\n")
    throw new Error("intentional full-rootfs failure boundary")
  },
})
