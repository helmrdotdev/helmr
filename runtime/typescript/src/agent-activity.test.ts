import assert from "node:assert/strict"
import { EventEmitter } from "node:events"
import { createServer } from "node:http"
import test from "node:test"
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

test("native handles cannot hide callback-created authored asynchronous work", async () => {
  const activity = new SessionActivity()
  const events = new EventEmitter()
  let timer: ReturnType<typeof setInterval> | undefined
  let received!: () => void
  const ready = new Promise<void>(resolve => { received = resolve })
  try {
    activity.authored(() => {
      events.once("data", () => { timer = setInterval(() => {}, 1000); received() })
      activity.native(() => setImmediate(() => events.emit("data")))
    })
    await ready
    assert.throws(() => activity.assertIdle(), /Timeout/)
    clearInterval(timer)
    await nextTurn()
    activity.assertIdle()
  } finally { clearInterval(timer); activity.close() }
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
