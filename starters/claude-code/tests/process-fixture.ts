import { EventEmitter } from "node:events"
import { PassThrough } from "node:stream"
import { beforeEach, mock } from "bun:test"

export function installProcessFixture() {
  let launchFailure: unknown
  beforeEach(() => { launchFailure = undefined })
  mock.module("node:child_process", () => ({ spawn: () => {
    const child = Object.assign(new EventEmitter(), {
      pid: 123, stdin: new PassThrough(), stdout: new PassThrough(), stderr: new PassThrough(),
      kill: () => { child.emit("close", 0); return true },
    })
    queueMicrotask(() => launchFailure ? child.emit("error", launchFailure) : child.emit("spawn"))
    return child
  } }))
  return { failLaunch: (error: unknown) => { launchFailure = error } }
}
