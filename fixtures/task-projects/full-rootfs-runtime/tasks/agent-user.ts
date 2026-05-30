import { task } from "@helmr/sdk"

import { agentSandbox } from "../shared/sandboxes"
import { assert, assertVisibleFile, command, readText } from "./_assert"

export const agentUser = task({
  id: "agent-user",
  sandbox: agentSandbox,
  maxDuration: 900,
  run: async (ctx) => {
    const username = (await command(["id", "-un"])).trim()
    const uid = (await command(["id", "-u"])).trim()

    assert(username === "agent", `expected agent user, got ${username}`)
    assert(uid === "10001", `expected uid 10001, got ${uid}`)
    assert(process.env["FOO"] === "BAR", "FOO env was not propagated to agent user")
    assert(
      process.env["PATH"] ===
        "/custom/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
      `PATH was overwritten for agent user: ${process.env["PATH"] ?? "<missing>"}`,
    )

    assert(
      (await readText("/workspace/editable/source-file.txt")) === "original source\n",
      "existing source file was not readable before edit",
    )
    await assertVisibleFile("/workspace/editable/source-file.txt", "edited by agent\n")
    await assertVisibleFile("/workspace/editable/subdir/new-file.txt", "created by agent\n")
    await assertVisibleFile("/tmp/agent-rootfs-write.txt", "rootfs only\n")

    return { runId: ctx.run.id, username, uid }
  },
})
