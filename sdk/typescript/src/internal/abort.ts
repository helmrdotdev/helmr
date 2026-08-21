export function abortableDelay(
  milliseconds: number,
  signal?: AbortSignal,
): Promise<void> {
  signal?.throwIfAborted()
  return new Promise((resolve, reject) => {
    const timer = setTimeout(done, milliseconds)
    function done(): void {
      signal?.removeEventListener("abort", aborted)
      resolve()
    }
    function aborted(): void {
      clearTimeout(timer)
      signal?.removeEventListener("abort", aborted)
      try {
        signal?.throwIfAborted()
      } catch (error) {
        reject(error)
        return
      }
      reject(new Error("Operation was aborted"))
    }
    signal?.addEventListener("abort", aborted, { once: true })
  })
}
