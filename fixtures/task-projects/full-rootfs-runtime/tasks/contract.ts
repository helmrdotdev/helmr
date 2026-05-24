import { task } from "@helmr/sdk"

import { contractSandbox } from "../shared/sandboxes"
import { assert, assertVisibleFile, command, readText } from "./_assert"

export const contract = task({
  id: "contract",
  sandbox: contractSandbox,
  maxDuration: 900,
  run: async (_payload, ctx) => {
    const curl = await command(["curl", "--version"])
    const usrBinCurl = await command(["/usr/bin/curl", "--version"])

    assert(curl.includes("curl"), "curl --version did not run from PATH")
    assert(usrBinCurl.includes("curl"), "/usr/bin/curl was not visible")
    assert(process.env["FOO"] === "BAR", "FOO env was not propagated")
    assert(
      process.env["PATH"] ===
        "/custom/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
      `PATH was overwritten: ${process.env["PATH"] ?? "<missing>"}`,
    )
    assert(process.cwd() === "/tmp/task", `expected /tmp/task cwd, got ${process.cwd()}`)
    assert((await readText("/workspace/precedence.txt")) === "source\n", "source tree lost priority")

    await assertVisibleFile("/tmp/task/scratch-cwd-write.txt", "scratch cwd\n")
    await assertVisibleFile("/tmp/scratch-tmp-write.txt", "scratch tmp\n")
    await assertVisibleFile("/var/log/scratch-log-write.txt", "scratch log\n")
    await assertVisibleFile(`${process.env["HOME"]}/.cache/scratch-cache-write.txt`, "scratch cache\n")
    await assertVisibleFile("/workspace/workspace-write.txt", "workspace\n")
    await assertVisibleFile("/workspace/local-tool-workspace-write.txt", "local tool\n")

    return {
      runId: ctx.run.id,
      curl: curl.split("\n")[0],
      usrBinCurl: usrBinCurl.split("\n")[0],
      cwd: process.cwd(),
      path: process.env["PATH"],
      hostedAgentRemoteFilesystem:
        "separate from the Helmr workspace; use the hosted agent SDK's own artifact channel",
    }
  },
})
