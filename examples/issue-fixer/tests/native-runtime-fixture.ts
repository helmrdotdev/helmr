import { EventEmitter } from "node:events"
import { PassThrough } from "node:stream"
import type { ChildProcessWithoutNullStreams } from "node:child_process"
import { beforeEach, afterEach } from "bun:test"
import { installNativeRuntime } from "../../../sdk/typescript/src/internal/native-runtime"
export function installNativeTestHooks(): { initializations: Promise<void>[]; operations: Promise<void>[]; failLaunch(error: unknown): void } {
const initializations: Promise<void>[] = []
const operations: Promise<void>[] = []
let launchFailure: unknown
let uninstall: (() => void) | undefined
beforeEach(() => {
  initializations.length = 0
  operations.length = 0
  launchFailure = undefined
  uninstall = installNativeRuntime({
    resource: lifecycle => { initializations.push(lifecycle.initialized); void lifecycle.initialized.catch(() => {}); return { signal: new AbortController().signal, cooperativeStopMs: 5000 } },
    spawn: () => {
      const child = Object.assign(new EventEmitter(), { stdin: new PassThrough(), stdout: new PassThrough(), stderr: new PassThrough(), kill: () => { child.emit("close", 0); return true } })
      const ready = launchFailure ? Promise.reject(launchFailure) : Promise.resolve({ scopeId: "fixture", processId: 123 })
      void ready.catch(() => {})
      return { child: child as unknown as ChildProcessWithoutNullStreams, ready }
    },
    register: (_turn, _resource, operation) => { operations.push(operation.done); void operation.done.catch(() => {}); return { run: callback => callback() } },
  })
})
afterEach(() => { uninstall?.() })
return { initializations, operations, failLaunch: error => { launchFailure = error } }
}
