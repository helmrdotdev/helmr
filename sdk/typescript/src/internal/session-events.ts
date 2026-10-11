import type { SessionEvent, SessionEventPage, SessionEventQuery } from "../contract"
import type { RequestOptions } from "../request"
import { abortableDelay } from "./abort"

export function sessionEvents(
  list: (query?: SessionEventQuery, options?: RequestOptions) => Promise<SessionEventPage>,
) {
  return Object.freeze({
    list,
    async *stream(query: SessionEventQuery = {}, options: RequestOptions = {}): AsyncIterableIterator<SessionEvent> {
      let after = query.after ?? 0
      for (;;) {
        options.signal?.throwIfAborted()
        const page = await list({ ...query, after }, options)
        for (const event of page.records) {
          options.signal?.throwIfAborted()
          yield event
        }
        after = page.nextAfter
        if (!page.hasMore) await abortableDelay(1000, options.signal)
      }
    },
  })
}
