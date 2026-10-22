import { agent } from "@helmr/sdk"
import { spawn } from "node:child_process"
import { randomUUID } from "node:crypto"
import { readFile, writeFile } from "node:fs/promises"
import { verificationComputer } from "../agent/agent"

// Both native and JavaScript work outlive a Turn. Their identities and progress
// must survive an explicit Computer checkpoint without rerunning initialization.
export const executionLifecycleAgent = agent({
  id: "verification-execution-lifecycle",
  template: verificationComputer,
  suspension: "manual",
  closeAfterIdle: "never",
  maxTurnDuration: "2m",
  init: async () => {
    const nonce = randomUUID()
    const path = `/root/background-${nonce}`
    await writeFile(path, "0")
    const child = spawn("sh", ["-c", 'n=0; while :; do n=$((n+1)); printf "%s" "$n" > "$1.tmp"; mv "$1.tmp" "$1"; sleep 0.2; done', "background", path], { stdio: "ignore" })
    if (!child.pid) throw new Error("Native background process did not start")
    const state = { nonce, pid: child.pid, path, ticks: 0 }
    setInterval(() => { state.ticks++ }, 200)
    return state
  },
  turn: async (_turn, context) => {
    const state = context.initResult
    process.kill(state.pid, 0)
    const nativeCount = Number(await readFile(state.path, "utf8"))
    return { nonce: state.nonce, pid: state.pid, ticks: state.ticks, nativeCount }
  },
})
