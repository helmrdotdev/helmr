import assert from "node:assert/strict"
import { once } from "node:events"
import { spawn } from "node:child_process"
import { createServer } from "node:http"
import test from "node:test"
import { randomBytes, randomUUID } from "node:crypto"
import { SessionActivity } from "./agent-activity"

const nextTurn = () => new Promise<void>(resolve => setImmediate(resolve))

test("closed authored listeners no longer pin compute", async () => {
  const activity = new SessionActivity()
  const server = activity.authored(() => createServer())
  try {
    await activity.authored(() => new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve)))
    assert.throws(() => activity.assertIdle(), /TCPSERVERWRAP/)
    await new Promise<void>((resolve, reject) => server.close(error => error ? reject(error) : resolve()))
    // The close callback precedes async_hooks destruction of the listener.
    await nextTurn()
    await nextTurn()
    activity.assertIdle()
  } finally { server.close(); activity.close() }
})

test("authored DNS lookups and their callback work pin compute", async () => {
  const activity = new SessionActivity()
  let timer: ReturnType<typeof setInterval> | undefined
  try {
    const { lookup } = await import("node:dns")
    const pending = activity.authored(() => new Promise<void>((resolve, reject) => {
      lookup("localhost", error => {
        if (error) { reject(error); return }
        timer = setInterval(() => {}, 1000)
        resolve()
      })
    }))
    assert.throws(() => activity.assertIdle(), /GETADDRINFOREQWRAP/)
    await pending
    await nextTurn()
    assert.throws(() => activity.assertIdle(), /Timeout/)
    clearInterval(timer)
    await nextTurn()
    activity.assertIdle()
  } finally { clearInterval(timer); activity.close() }
})

test("ordinary child stdio can be retained but callback-created work pins compute", async () => {
  const activity = new SessionActivity()
  let timer: ReturnType<typeof setInterval> | undefined
  const child = activity.authored(() => spawn(process.execPath, ["-e", "process.stdin.on('data', () => process.stdout.write('ready'))"], { stdio: "pipe" }))
  try {
    await once(child, "spawn")
    await nextTurn()
    activity.assertIdle()
    const received = once(child.stdout, "data")
    child.stdout.once("data", () => { timer = setInterval(() => {}, 1000) })
    child.stdin.write("go")
    await received
    assert.throws(() => activity.assertIdle(), /Timeout/)
    clearInterval(timer)
    await nextTurn()
    activity.assertIdle()
  } finally {
    clearInterval(timer)
    child.kill()
    await once(child, "close")
    activity.close()
  }
})

test("inert bound context does not pin compute but its later work does", async () => {
  const activity = new SessionActivity()
  let timer: ReturnType<typeof setInterval> | undefined
  try {
    const { AsyncResource } = await import("node:async_hooks")
    const callback = activity.authored(() => AsyncResource.bind(() => { timer = setInterval(() => {}, 1000) }))
    activity.assertIdle()
    callback()
    assert.throws(() => activity.assertIdle(), /Timeout/)
    clearInterval(timer)
    await nextTurn()
    activity.assertIdle()
  } finally { clearInterval(timer); activity.close() }
})


test("completed synchronous randomness does not hide pending asynchronous randomness", async () => {
  const activity = new SessionActivity()
  let timer: ReturnType<typeof setInterval> | undefined
  try {
    activity.authored(() => { randomUUID({ disableEntropyCache: true }); randomBytes(32) })
    activity.assertIdle()
    const pending = activity.authored(() => new Promise<void>((resolve, reject) => {
      randomBytes(32, error => {
        if (error) { reject(error); return }
        timer = setInterval(() => {}, 1000)
        resolve()
      })
    }))
    assert.throws(() => activity.assertIdle(), /RANDOMBYTESREQUEST/)
    await pending
    await nextTurn()
    assert.throws(() => activity.assertIdle(), /Timeout/)
    clearInterval(timer)
    await nextTurn()
    activity.assertIdle()
  } finally { clearInterval(timer); activity.close() }
})
