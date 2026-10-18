import assert from "node:assert/strict"
import { test } from "node:test"
import { mkdtemp, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { OpenCode } from "@opencode/sdk/effect"
import { Session } from "@opencode/core/session"
import { SessionRestart } from "@opencode/core/session/execution/restart"
import { SessionStore } from "@opencode/core/session/store"
import { SessionInbox } from "@opencode/core/session/inbox"
import { SessionMessage } from "@opencode/core/session/message"
import { Job } from "@opencode/core/job"
import { Location } from "@opencode/schema/location"
import { Context, Effect, Layer, Schema } from "effect"
import { recovery } from "../agents/recovery"

// A real persisted native database represents an owner lost before it could
// finish. Use the shipped startup composition, not a test-only recovery path.
test("OpenCode recovery retires all owner-database input and converges after partial retirement", { timeout: 30_000 }, async () => {
  const root = await mkdtemp(join(tmpdir(), "helmr-opencode-recovery-"))
  const options: OpenCode.CreateOptions = { database: { path: join(root, "native.sqlite") }, config: { directory: join(root, "config"), project: false, claude: false }, models: { fetch: false, snapshot: false }, fs: { filewatcher: false, fff: false }, plugins: [] }
  let sessions!: Session.Interface, store!: SessionStore.Interface, inbox!: Context.Service.Shape<typeof SessionInbox.Service>, jobs!: Job.Interface
  let fail = false, retired = 0
  const overrides = [
    Session.node.replace(Session.node.mapLayer(layer => Layer.flatMap(layer, context => {
      const native = Context.get(context, Session.Service)
      sessions = native
      return Layer.succeedContext(Context.add(context, Session.Service, { ...native, cancelInbox: input => Effect.gen(function* () {
        yield* native.cancelInbox(input)
        retired++
        if (fail) { fail = false; yield* Effect.die(new Error("injected retirement failure")) }
      }) }))
    }))),
    SessionStore.node.replace(SessionStore.node.mapLayer(layer => Layer.tap(layer, context => Effect.sync(() => { store = Context.get(context, SessionStore.Service) })))),
    SessionInbox.node.replace(SessionInbox.node.mapLayer(layer => Layer.tap(layer, context => Effect.sync(() => { inbox = Context.get(context, SessionInbox.Service) })))),
    Job.node.replace(Job.node.mapLayer(layer => Layer.tap(layer, context => Effect.sync(() => { jobs = Context.get(context, Job.Service) })))),
  ]
  const inert = SessionRestart.node.replace(Layer.succeed(SessionRestart.Service, { resumeSuspendedSessions: Effect.void }))
  let ids: Session.ID[] = []
  try {
    await Effect.runPromise(Effect.scoped(Effect.gen(function* () {
      const sdk = yield* OpenCode.create(options, { overrides: [inert, ...overrides] })
      const parent = yield* sdk.sessions.create({ location: Schema.decodeUnknownSync(Location.Ref)({ directory: root }) })
      const child = yield* sdk.sessions.create({ parentID: parent.id })
      const idle = yield* sdk.sessions.create({ location: Schema.decodeUnknownSync(Location.Ref)({ directory: root }) })
      const completedChild = yield* sdk.sessions.create({ parentID: idle.id })
      ids = [parent.id, child.id, idle.id, completedChild.id]
      yield* store.claim(parent.id)
      yield* store.claim(child.id)
      for (const id of [parent.id, child.id]) {
        for (const delivery of ["queue", "steer"] as const) yield* inbox.admit({ id: SessionMessage.ID.create(), sessionID: id, item: { type: "user", delivery, payload: { text: `retired ${delivery}` } } })
      }
      yield* inbox.admitCompaction({ id: SessionMessage.ID.create(), sessionID: child.id, delivery: "queue" })
      for (const [owner, childID] of [[parent.id, child.id], [idle.id, completedChild.id]]) {
        const id = `background-${owner}`
        yield* jobs.start({ id, type: "subagent", notificationID: SessionMessage.ID.create(), recovery: { kind: "subagent", parentSessionID: owner!, childSessionID: childID!, agent: "fixture", description: "completed fixture child" }, run: Effect.succeed("completed fixture") })
        yield* jobs.wait({ id })
        yield* jobs.background(id)
      }
    })))
    fail = true
    await Effect.runPromise(Effect.scoped(Effect.gen(function* () {
      const startup = recovery()
      yield* OpenCode.create(options, { overrides: [startup.replacement, ...overrides] })
      const outcome = yield* Effect.exit(Effect.tryPromise(() => startup.ready))
      assert.equal(outcome._tag, "Failure")
      assert.equal(retired, 1)
      assert.equal((yield* store.listSuspended()).length, 0)
    })))
    for (let reopen = 0; reopen < 2; reopen++) {
      await Effect.runPromise(Effect.scoped(Effect.gen(function* () {
        const startup = recovery()
        yield* OpenCode.create(options, { overrides: [startup.replacement, ...overrides] })
        yield* Effect.promise(() => startup.ready)
        assert.equal((yield* store.listSuspended()).length, 0)
        assert.equal((yield* jobs.pendingBackground).length, 0)
        for (const id of ids) assert.equal((yield* sessions.inbox(id)).length, 0)
        assert.ok(retired >= 7)
      })))
    }
  } finally { await rm(root, { recursive: true, force: true }) }
})
