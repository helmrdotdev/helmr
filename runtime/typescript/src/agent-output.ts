import type { OutputReceipt } from "../../../sdk/typescript/src/agent"
import { ContentError, CONTENT_BYTES, CONTENT_PARTS, contentBytes, normalizeContent, type Content, type ContentPart, type HumanContent } from "../../../sdk/typescript/src/content"

function wait<T>(pending: Promise<T>, signal: AbortSignal): Promise<T> {
  return new Promise((resolve, reject) => {
    const abort = () => reject(signal.reason)
    signal.addEventListener("abort", abort, { once: true })
    void pending.then(resolve, reject).finally(() => signal.removeEventListener("abort", abort))
    if (signal.aborted) abort()
  })
}

// One writer orders direct writes, pipe batches and publication admissions. The
// caller owns source ordering; a write receipt acknowledges its own durable batch.
export class AgentOutputWriter {
  private tail: Promise<unknown> = Promise.resolve()
  private readonly pending = new Set<Promise<unknown>>()
  private batch: ContentPart[] = []
  private timer: ReturnType<typeof setTimeout> | undefined
  private pipeAbort: AbortController | undefined
  private readonly send: (content: Content) => Promise<OutputReceipt>
  constructor(send: (content: Content) => Promise<OutputReceipt>) { this.send = send }

  write(value: HumanContent): Promise<OutputReceipt> {
    const content = normalizeContent(value)
    void this.flush().catch(() => {})
    return this.enqueue(() => this.send(content))
  }

  // Called synchronously at ask/respond invocation, before future fragments can
  // enter the queue. Only the received prefix belongs ahead of that admission.
  ordered<T>(action: () => Promise<T>): Promise<T> {
    const flushed = this.flush()
    const prefix = [...this.pending, flushed]
    return this.enqueue(async () => { await Promise.all(prefix); return action() })
  }

  async pipe(values: AsyncIterable<HumanContent>, signal: AbortSignal, cleanup: (pending: Promise<unknown>) => Promise<unknown>): Promise<void> {
    if (this.pipeAbort) throw new ContentError("invalid_arguments", "Serialize concurrent output producers")
    const abort = new AbortController()
    this.pipeAbort = abort
    const stopped = AbortSignal.any([signal, abort.signal])
    const iterator = values[Symbol.asyncIterator]()
    let exhausted = false
    try {
      for (;;) {
        stopped.throwIfAborted()
        // A timer may have flushed while next() was pending. Keep at most that
        // one incoming value, and do not pull again until durability is known.
        await wait(Promise.all([...this.pending]), stopped)
        const next = await wait(Promise.resolve(iterator.next()), stopped)
        stopped.throwIfAborted()
        if (next.done) { exhausted = true; await this.flush(); await wait(Promise.all([...this.pending]), stopped); return }
        const content = normalizeContent(next.value)
        await wait(Promise.all([...this.pending]), stopped)
        let combined = this.combine(this.batch, content)
        if (combined.length > CONTENT_PARTS || contentBytes(combined) > CONTENT_BYTES) {
          await this.flush()
          combined = [...content]
        }
        this.batch = combined
        if (this.batch.length === 0) continue
        if (contentBytes(this.batch) >= 16 * 1024) await this.flush()
        else if (this.timer === undefined) this.timer = setTimeout(() => { void this.flush().catch(() => {}) }, 100)
      }
    } finally {
      clearTimeout(this.timer); this.timer = undefined
      this.batch = []
      this.pipeAbort = undefined
      if (!exhausted) await cleanup(Promise.resolve(iterator.return?.()))
    }
  }

  private combine(before: Content, after: Content): ContentPart[] {
    const combined = [...before]
    for (const part of after) {
      const last = combined.at(-1)
      if (last?.type === "text" && part.type === "text") combined[combined.length - 1] = { type: "text", text: last.text + part.text }
      else combined.push(part)
    }
    return combined
  }

  private flush(): Promise<void> {
    clearTimeout(this.timer); this.timer = undefined
    if (this.batch.length === 0) return Promise.resolve()
    const content = this.batch
    this.batch = []
    const owner = this.pipeAbort
    const pending = this.enqueue(() => this.send(content)).then(() => {})
    void pending.catch(error => owner?.abort(error))
    return pending
  }

  private enqueue<T>(action: () => Promise<T>): Promise<T> {
    const pending = this.tail.then(action)
    this.pending.add(pending)
    this.tail = pending.then(() => {}, () => {})
    void pending.then(() => this.pending.delete(pending), () => this.pending.delete(pending))
    return pending
  }
}
