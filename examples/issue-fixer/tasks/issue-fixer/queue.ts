// One consumer; used for native messages, never as a managed durable wait.
export class Queue<T> implements AsyncIterable<T> {
  private values: T[] = []
  private wake?: () => void
  private ended = false
  private error?: Error
  push(value: T): void {
    if (this.ended) throw new Error("Stream is closed")
    this.values.push(value)
    this.wake?.()
  }
  end(error?: Error): void {
    this.ended = true
    this.error = error
    this.wake?.()
  }
  async *[Symbol.asyncIterator](): AsyncIterator<T> {
    for (;;) {
      if (this.error) throw this.error
      if (this.values.length) { yield this.values.shift()!; continue }
      if (this.ended) return
      await new Promise<void>(resolve => { this.wake = resolve })
      this.wake = undefined
    }
  }
}
