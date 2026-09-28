import type { CursorPage } from "./contract"
import type { RequestOptions } from "./request"
import { abortableDelay } from "./internal/abort"
import { timestampString } from "./internal/timestamp"

export interface CommandLogQuery { readonly cursor?: string; readonly limit?: number }
export interface CommandLogStreamQuery { readonly after?: string }
export type CommandLogRecord = Readonly<{ cursor: string; stream: "stdout" | "stderr" }> & (
  | Readonly<{ kind: "output"; content: Uint8Array; observedAt: string }>
  | Readonly<{ kind: "gap"; fromSequence: string; throughSequence: string }>
)

export function encodeCommandLogQuery(query: CommandLogQuery = {}): CommandLogQuery {
  if (query.cursor !== undefined && (typeof query.cursor !== "string" || query.cursor.length === 0 || query.cursor.length > 4096)) throw new Error("Command log cursor is invalid")
  if (query.limit !== undefined && (!Number.isInteger(query.limit) || query.limit < 1 || query.limit > 100)) throw new Error("Command log limit must be in [1,100]")
  return { ...query }
}

export interface CommandLogReadPage extends CursorPage<CommandLogRecord> { readonly outputState: "open" | "closed" | "unavailable" }
export function parseCommandLogPage(value: unknown): CommandLogReadPage {
  const raw = object(value)
  const outputState = raw["output_state"]
  if (outputState !== "open" && outputState !== "closed" && outputState !== "unavailable") throw new Error("Command log output state is invalid")
  if (!Array.isArray(raw["logs"])) throw new Error("Command logs must be an array")
  const items = raw["logs"].map((item): CommandLogRecord => {
    const record = object(item)
    const cursor = record["cursor"], stream = record["stream"]
    if (typeof cursor !== "string" || cursor.length === 0 || (stream !== "stdout" && stream !== "stderr")) throw new Error("Command log identity is invalid")
    if (record["kind"] === "gap") {
      const from = record["from_sequence"], through = record["through_sequence"]
      if (typeof from !== "string" || typeof through !== "string" || !/^(0|[1-9][0-9]*)$/.test(from) || !/^(0|[1-9][0-9]*)$/.test(through) || BigInt(from) > BigInt(through)) throw new Error("Command log gap is invalid")
      return Object.freeze({ kind: "gap", cursor, stream, fromSequence: from, throughSequence: through })
    }
    if (record["kind"] !== "output") throw new Error("Command log kind is invalid")
    const encoded = record["content_base64"] ?? ""
    if (typeof encoded !== "string") throw new Error("Command log content is invalid")
    const binary = atob(encoded)
    if (btoa(binary) !== encoded) throw new Error("Command log content is not canonical base64")
    return Object.freeze({ kind: "output", cursor, stream, content: Uint8Array.from(binary, c => c.charCodeAt(0)), observedAt: timestampString(record["observed_at"], "Command log.observed_at") })
  })
  const next = raw["next_cursor"]
  if (next !== undefined && (typeof next !== "string" || next.length === 0)) throw new Error("Command log next cursor is invalid")
  if (items.length > 0 && next !== items.at(-1)!.cursor) throw new Error("Command log page cursor does not match its records")
  if (items.length === 0 && next !== undefined) throw new Error("Empty Command log page advanced its cursor")
  return Object.freeze({ outputState, items: Object.freeze(items), ...(next === undefined ? {} : { nextCursor: next as string }) })
}

export async function* streamCommandLogs(
  read: (query: CommandLogQuery, signal?: AbortSignal) => Promise<CommandLogReadPage>,
  query: CommandLogStreamQuery = {},
  options: RequestOptions = {},
): AsyncGenerator<CommandLogRecord> {
  let cursor = encodeCommandLogQuery({ ...(query.after === undefined ? {} : { cursor: query.after }) }).cursor
  for (;;) {
    options.signal?.throwIfAborted()
    let page: CommandLogReadPage
    try {
      page = await read({ ...(cursor === undefined ? {} : { cursor }) }, options.signal)
    } catch (error) {
      options.signal?.throwIfAborted()
      if (!(error instanceof Error) || !("code" in error) || error.code !== "telemetry_lagging") throw error
      await abortableDelay(1000, options.signal)
      continue
    }
    for (const record of page.items) {
      options.signal?.throwIfAborted()
      if (record.cursor === cursor) throw new Error("Command log cursor did not advance")
      cursor = record.cursor
      yield record
    }
    options.signal?.throwIfAborted()
    if (page.items.length > 0) continue
    if (page.outputState === "closed") return
    if (page.outputState === "unavailable") throw Object.assign(new Error("Command output completeness is unavailable after interrupted execution"), { code: "command_output_unavailable" })
    await abortableDelay(1000, options.signal)
  }
}

function object(value: unknown): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) throw new Error("Command log response must be an object")
  return value as Record<string, unknown>
}
