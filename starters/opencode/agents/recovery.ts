import { Bus } from "@opencode/core/bus"
import { Job } from "@opencode/core/job"
import { SessionExecution } from "@opencode/core/session/execution"
import { LayerNode } from "@opencode/util/effect/layer-node"
import { Session } from "@opencode/core/session"
import { SessionRestart } from "@opencode/core/session/execution/restart"
import { SessionStore } from "@opencode/core/session/store"
import { Effect, Exit, Layer } from "effect"

// Helmr has already contained the former process before reconstructing a Session.
// Native terminalization keeps history, but must not resume interrupted input.
export function recovery() {
  let resolve!: () => void, reject!: (error: unknown) => void
  const ready = new Promise<void>((yes, no) => { resolve = yes; reject = no })
  void ready.catch(() => {})
  const layer = Layer.unwrap(Effect.gen(function* () {
    const sessions = yield* Session.Service
    const store = yield* SessionStore.Service
    const native = SessionRestart.layer({ maxAttempts: 0 }).pipe(Layer.provide(Layer.succeed(Session.Service, {
      ...sessions,
      synthetic: input => sessions.synthetic({ ...input, resume: false }),
    })))
    return Layer.effect(SessionRestart.Service, Effect.gen(function* () {
      const restart = yield* SessionRestart.Service
      return {
        resumeSuspendedSessions: Effect.gen(function* () {
          yield* restart.resumeSuspendedSessions
          // This database belongs only to this Helmr Session, including its native
          // children and idle recipients of recovery notifications.
          for (const session of yield* store.list()) {
            for (const item of yield* sessions.inbox(session.id)) {
              yield* sessions.cancelInbox({ sessionID: session.id, inboxID: item.id })
            }
          }
        }).pipe(Effect.orDie, Effect.onExit(exit => Effect.sync(() => {
          if (Exit.isSuccess(exit)) resolve()
          else reject(new Error("OpenCode recovery did not settle", { cause: exit.cause }))
        }))),
      }
    })).pipe(Layer.provide(native))
  }))
  const replacement = SessionRestart.node.replace(LayerNode.make({
    service: SessionRestart.Service, tag: SessionRestart.node.tag, layer,
    deps: [Bus.node, Job.node, SessionExecution.node, Session.node, SessionStore.node],
  }))
  return { replacement, ready }
}
