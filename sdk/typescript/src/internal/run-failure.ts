import type { RunFailure } from "../contract"

export function runFailureError(failure: RunFailure): Error {
  const error = new Error(failure.message) as Error & {
    code: string
    details: RunFailure["details"]
  }
  error.name = "RunFailure"
  error.code = failure.code
  error.details = failure.details
  return error
}
