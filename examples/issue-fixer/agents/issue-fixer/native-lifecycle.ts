import { registerNativeResource, type NativeResource } from "@helmr/sdk/internal"

interface Harness {
  readonly reusable: boolean
  readonly idle: boolean
  close(): Promise<void>
}

// Registration happens synchronously before opening files or awaiting a native
// handshake, so setup cannot accidentally leave an untracked initialization.
export function openNativeHarness<T extends Harness>(open: (resource: NativeResource) => Promise<T>): Promise<T> {
  let harness: T | undefined
  let initialized = false
  let resolve!: () => void, reject!: (error: unknown) => void
  const ready = new Promise<void>((yes, no) => { resolve = yes; reject = no })
  let opening: Promise<T>
  const resource = registerNativeResource({
    initialized: ready,
    state: () => ({ phase: !initialized ? "initializing" : !harness!.reusable ? "closed" : harness!.idle ? "idle" : "active", reusable: initialized && harness!.reusable }),
    stop: async () => { const opened = await opening.catch(() => undefined); await opened?.close() },
  })
  opening = Promise.resolve().then(() => open(resource)).then(value => { harness = value; initialized = true; resolve(); return value }, error => { reject(error); throw error })
  void opening.catch(() => {})
  return opening
}
