import type {
  JsonValue,
  OutputReadOptions,
  RuntimeSessionOperationOptions,
  RuntimeSessionRef,
  RuntimeSessions,
  SendOptions,
} from "./contract"
import { resourceID } from "./internal/id"
import { currentRuntimeOperations } from "./internal/runtime"

export function createRuntimeSessionRef(id: string): RuntimeSessionRef {
  const sessionID = resourceID(id, "Session ID")
  return Object.freeze({
    id: sessionID,
    input: Object.freeze({
      send(input: JsonValue, options?: SendOptions) {
        return currentRuntimeOperations().actorInputSend(sessionID, input, options)
      },
    }),
    output: Object.freeze({
      async *read(options?: OutputReadOptions) {
        let after = options?.after
        for (;;) {
          if (options?.signal?.aborted) throw options.signal.reason
          const page = await currentRuntimeOperations().sessionOutputPage(sessionID, {
            ...(after === undefined ? {} : { after }),
            ...(options?.limit === undefined ? {} : { limit: options.limit }),
            ...(options?.signal === undefined ? {} : { signal: options.signal }),
          })
          for (const record of page.records) yield record
          if (!page.hasMore) return
          after = page.nextAfter
        }
      },
      async list(options?: OutputReadOptions) {
        return (await currentRuntimeOperations().sessionOutputPage(sessionID, options)).records
      },
    }),
    status() {
      return currentRuntimeOperations().sessionStatus(sessionID)
    },
    close(options?: RuntimeSessionOperationOptions) {
      return currentRuntimeOperations().sessionClose(sessionID, options)
    },
  })
}

export const sessions: RuntimeSessions = Object.freeze({
  ref(id: string) {
    return createRuntimeSessionRef(id)
  },
})
