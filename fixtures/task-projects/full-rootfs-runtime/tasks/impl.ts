import { task, type PayloadSchema } from "@helmr/sdk"
import { writeFile } from "node:fs/promises"

import { implSandbox } from "../shared/sandboxes"
import { assert, readText } from "./_assert"

const payloadSchema: PayloadSchema<{ readonly label?: string }, { readonly label?: string }> = {
  "~standard": {
    version: 1,
    vendor: "fixture",
    validate(value) {
      if (value === undefined || value === null) {
        return { value: {} }
      }
      if (typeof value !== "object") {
        return { issues: [{ message: "expected object" }] }
      }
      const label = (value as Record<string, unknown>)["label"]
      if (label !== undefined && typeof label !== "string") {
        return { issues: [{ message: "expected string", path: ["label"] }] }
      }
      return { value: label === undefined ? {} : { label } }
    },
  },
}

export const impl = task({
  id: "impl",
  sandbox: implSandbox,
  maxDuration: 900,
  payloadSchema,
  run: async (payload, ctx) => {
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
