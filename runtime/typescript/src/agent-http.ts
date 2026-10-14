import { Agent, getGlobalDispatcher, setGlobalDispatcher } from "undici"
import { isIP } from "node:net"

// Local native services can legitimately wait for a model, a tool or a person.
// Keep connection establishment bounded and let caller signals bound a request.
// An explicit dispatcher remains entirely under the application's control.
export function installSessionHttp(): () => void {
  const previous = getGlobalDispatcher()
  const agent = new Agent()
  const dispatcher = agent.compose(dispatch => (options, handler) => {
    const host = new URL(String(options.origin)).hostname
    const loopback = host === "localhost" || host === "[::1]" || (isIP(host) === 4 && host.startsWith("127."))
    return dispatch(loopback ? { ...options, headersTimeout: 0, bodyTimeout: 0 } : options, handler)
  })
  setGlobalDispatcher(dispatcher)
  return () => {
    setGlobalDispatcher(previous)
    void agent.destroy().catch(() => {})
  }
}
