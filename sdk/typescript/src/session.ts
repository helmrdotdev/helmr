import type {
  JsonValue,
  SessionCloseRequest,
  SessionInputSendRequest,
  SessionOutputQuery,
  SessionRef,
} from "./contract"
import { resourceID } from "./internal/id"
import { currentRuntimeOperations } from "./internal/runtime"
import type { RequestOptions } from "./request"

export function createRuntimeSessionRef(id: string): SessionRef {
  const sessionID = resourceID(id, "Session ID")
  return Object.freeze({
    id: sessionID,
    input: Object.freeze({
      send(
        input: JsonValue,
        request?: SessionInputSendRequest,
        options?: RequestOptions,
      ) {
        return currentRuntimeOperations().actorInputSend(
          sessionID,
          input,
          request,
          options?.signal,
        )
      },
    }),
    output: Object.freeze({
      list(query?: SessionOutputQuery, options?: RequestOptions) {
        return currentRuntimeOperations().sessionOutputPage(
          sessionID,
          query,
          options?.signal,
        )
      },
    }),
    retrieve(options: Readonly<{ signal?: AbortSignal }> = {}) {
      return currentRuntimeOperations().sessionRetrieve(sessionID, options.signal)
    },
    close(request?: SessionCloseRequest, options?: RequestOptions) {
      return currentRuntimeOperations().sessionClose(
        sessionID,
        request,
        options?.signal,
      )
    },
  })
}

export const sessions = Object.freeze({
  ref(id: string) {
    return createRuntimeSessionRef(id)
  },
})
