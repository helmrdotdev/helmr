import type { ClientSessionRef } from "./client-session"
import type { TurnOutcome, TimedTurnWaitResult, TurnWaitOptions } from "./contract"

declare const session: ClientSessionRef
declare const options: TurnWaitOptions
const turn = session.turn("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc34")
const outcome: Promise<TurnOutcome> = turn.wait()
const observed: Promise<TimedTurnWaitResult> = turn.wait({ timeout: "1m" })
const dynamic: Promise<TurnOutcome | TimedTurnWaitResult> = turn.wait(options)
// @ts-expect-error An observation timeout is not a Turn outcome.
const terminal: Promise<TurnOutcome> = turn.wait({ timeout: 100 })
void outcome; void observed; void dynamic; void terminal
