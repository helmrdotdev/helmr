import { spawn } from "node:child_process"
import type { BuildContext, ComputerDefinition } from "@helmr/sdk"

// The guest owns the enclosing cgroup and joins all descendants before capture.
// This driver supplies ordinary process execution to the authored prepare hook.
export async function runComputerPreparation(computer: ComputerDefinition, signal: AbortSignal): Promise<void> {
  signal.throwIfAborted()
  const cancellation = new AbortController()
  const executionSignal = AbortSignal.any([signal, cancellation.signal])
  const pending = new Set<Promise<void>>()
  let closed = false
  const build: BuildContext = {
    signal: executionSignal,
    exec(command, options) {
      if (closed) return Promise.reject(new Error("Computer preparation has finished"))
      executionSignal.throwIfAborted()
      const argv = typeof command === "string" ? ["/bin/sh", "-c", command] : [...command]
      if (!argv.length || !argv[0] || argv.some(argument => typeof argument !== "string")) return Promise.reject(new TypeError("Preparation command requires an executable"))
      const task = new Promise<void>((resolve, reject) => {
        let failure: Error | undefined
        const child = spawn(argv[0]!, argv.slice(1), {
          cwd: options?.cwd,
          env: options?.env ? { ...process.env, ...options.env } : process.env,
          stdio: ["ignore", "inherit", "inherit"],
          signal: executionSignal,
          killSignal: "SIGKILL",
        })
        child.once("error", error => { failure = error })
        child.once("close", (code, killedBy) => {
          if (failure) reject(failure)
          else if (code === 0) resolve()
          else reject(new Error(killedBy ? `Preparation command terminated by ${killedBy}` : `Preparation command exited with status ${code}`))
        })
      }).finally(() => { pending.delete(task) })
      pending.add(task)
      return task
    },
  }
  try {
    await computer.prepare?.(build)
    closed = true
    await Promise.all(pending)
    executionSignal.throwIfAborted()
  } finally {
    closed = true
    cancellation.abort()
    await Promise.allSettled(pending)
  }
}
