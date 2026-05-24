import { task } from "@helmr/sdk"
import { writeFile } from "node:fs/promises"

import { implSandbox } from "../shared/sandboxes"
import { assert, readText } from "./_assert"

export const impl = task({
  id: "impl",
  sandbox: implSandbox,
  maxDuration: 900,
  run: async (payload: { label?: string } | undefined, ctx) => {
    const packageJson = await readText("/workspace/package.json")
    const sourceText = await readText("/workspace/precedence.txt")
    const installInput = await readText("/opt/helmr-deps/install-input.sha256")
    const installLog = await readText("/opt/helmr-deps/install.log")

    assert(packageJson.includes('"full-rootfs-runtime"'), "workspace package.json missing")
    assert(sourceText.startsWith("source"), "GitHub checkout workspace was not mounted")
    assert(installInput.includes("package.json"), "package.json was not copied into image input")
    assert(installLog === "install layer executed\n", "image run layer did not execute")

    const label = payload?.label ?? "impl"
    await writeFile("/workspace/generated.txt", `${label}:${ctx.run.id}\n`)
    return { runId: ctx.run.id, label, sourceText, installInput }
  },
})
