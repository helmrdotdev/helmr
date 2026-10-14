import { AsyncLocalStorage, AsyncResource, createHook } from "node:async_hooks"
import { type ChildProcess } from "node:child_process"
import { channel } from "node:diagnostics_channel"
import { getServers } from "node:dns"

// Handle identity is inspected only in the pinned managed Node runtime. Missing
// or changed Node fields fail closed: an unknown handle continues to pin compute.
type NodeHandleOwner = { _handle?: object | null }

export class SessionActivity {
  private readonly authoredScope = new AsyncLocalStorage<boolean>()
  private readonly resources = new Map<number, { type: string; resource: WeakRef<object> }>()
  private readonly children = new Set<ChildProcess>()
  private readonly childChannel = channel("child_process")
  private readonly onChild = (message: unknown) => {
    if (!this.authoredScope.getStore()) return
    const child = (message as { process: ChildProcess }).process
    this.children.add(child)
    child.once("close", () => this.children.delete(child))
  }
  private readonly hook = createHook({
    init: (id, type, _trigger, resource) => {
      // Promise reachability does not imply runnable work. Timers, I/O and
      // microtask callbacks that could resolve them have their own resources.
      if (type !== "PROMISE" && !(resource instanceof AsyncResource) && this.authoredScope.getStore()) {
        this.resources.set(id, { type, resource: new WeakRef(resource) })
      }
    },
    destroy: id => { this.resources.delete(id) },
  })
  constructor() {
    void process.stdout; void process.stderr
    getServers()
    this.childChannel.subscribe(this.onChild)
    this.hook.enable()
  }
  authored<T>(action: () => T): T { return this.authoredScope.run(true, action) }
  assertIdle(): void {
    const retained = new Set<object>()
    for (const child of this.children) {
      // Child watchers and their exact stdio pipes are captured with the entire
      // Session process tree. A pipe callback's timers, sockets and writes are
      // separate resources and are never qualified by this relationship.
      const processHandle = (child as unknown as NodeHandleOwner)._handle
      if (processHandle) retained.add(processHandle)
      for (const stream of child.stdio ?? []) {
        const handle = (stream as NodeHandleOwner | null)?._handle
        if (handle) retained.add(handle)
      }
    }
    const pending = new Set<string>()
    for (const { type, resource } of this.resources.values()) {
      const handle = resource.deref()
      // Node 24.21 creates a RandomBytesJob for synchronous randomFill/UUIDs
      // too; only asynchronous jobs have ondone. Synchronous jobs can survive
      // until GC despite having already returned. Never exclude pending jobs.
      if (type === "RANDOMBYTESREQUEST" && handle && !("ondone" in handle)) continue
      if ((type === "PROCESSWRAP" || type === "PIPEWRAP") && handle && retained.has(handle)) continue
      pending.add(type)
    }
    if (pending.size) throw new Error(`Session has unqualified asynchronous resources: ${[...pending].sort().join(", ")}`)
  }
  close(): void {
    this.hook.disable()
    this.childChannel.unsubscribe(this.onChild)
    this.resources.clear()
    this.children.clear()
    this.authoredScope.disable()
  }
}
