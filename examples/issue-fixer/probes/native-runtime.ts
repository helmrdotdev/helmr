// Explicit local-probe transport. Production always uses the guest-owned proxy;
// these tests prove native protocol behavior and make no containment claim.
import { spawn } from "node:child_process"
import { randomUUID } from "node:crypto"
import { SessionNativeRegistry } from "../../../runtime/typescript/src/agent-native"
import { installNativeRuntime } from "../../../sdk/typescript/src/internal/native-runtime"
export function localNativeRuntime(signal: AbortSignal) {
  const registry = new SessionNativeRegistry(signal)
  const uninstall = installNativeRuntime({
    resource: lifecycle => registry.resource(lifecycle),
    register: (turn, resource, operation) => registry.register(turn, resource, operation),
    spawn: (resource, command, args, options) => {
      const child = spawn(command, [...args], { ...options, signal: options.signal ? AbortSignal.any([resource.signal, options.signal]) : resource.signal, stdio: ["pipe", "pipe", "pipe"] })
      return { child, ready: child.pid ? Promise.resolve({ scopeId: randomUUID(), processId: child.pid }) : Promise.reject(new Error("Local native process did not start")) }
    },
  })
  return { registry, uninstall }
}
