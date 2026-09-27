// Bun 1.3.13 cancels AbortSignal.timeout when its last listener is removed,
// even when polling adds a new listener. Own the timer across request boundaries.
export function deadline(milliseconds: number): AbortSignal {
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(new DOMException("Verification deadline exceeded", "TimeoutError")), milliseconds)
  timer.unref()
  return controller.signal
}
