import { AsyncLocalStorage, AsyncResource, createHook } from "node:async_hooks"
import { getServers } from "node:dns"

// Unknown authored asynchronous resources pin healthy compute. Native child
// pipes are separately qualified by the adapter and guest process supervisor.
export class SessionActivity {
  private readonly authoredScope = new AsyncLocalStorage<boolean>()
  private nativeCreationDepth = 0
  private readonly resources = new Map<number, string>()
  private readonly hook = createHook({
    init: (id, type, _trigger, resource) => {
      // Promise reachability does not imply runnable work. Timers, I/O and
      // microtask callbacks that could resolve them have their own resources.
      if (type !== "PROMISE" && !(resource instanceof AsyncResource) && this.nativeCreationDepth === 0 && this.authoredScope.getStore()) this.resources.set(id, type)
    },
    destroy: id => { this.resources.delete(id) },
  })
  constructor() {
    // Initialize runtime-owned output handles before marking authored work.
    void process.stdout; void process.stderr
    // Initialize the process-wide resolver, not a lookup. Authored lookups
    // still create separately tracked request resources.
    getServers()
    this.hook.enable()
  }
  authored<T>(action: () => T): T { return this.authoredScope.run(true, action) }
  native<T>(action: () => T): T {
    // Exclude only handles created by the synchronous owned spawn, preserving
    // authored context in their callbacks. Descendant timers/I/O remain tracked.
    this.nativeCreationDepth++
    try { return action() } finally { this.nativeCreationDepth-- }
  }
  assertIdle(): void {
    if (this.resources.size) throw new Error(`Session has unqualified asynchronous resources: ${[...new Set(this.resources.values())].sort().join(", ")}`)
  }
  close(): void { this.hook.disable(); this.resources.clear(); this.authoredScope.disable() }
}
