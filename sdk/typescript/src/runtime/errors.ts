export class RunNotFoundError extends Error {
  readonly runId: string

  constructor(runId: string) {
    super(`run ${runId} was not found`)
    this.name = "RunNotFoundError"
    this.runId = runId
  }
}

export class AuthError extends Error {
  constructor(message: string) {
    super(message)
    this.name = "AuthError"
  }
}

export class TimeoutError extends Error {
  constructor(message: string) {
    super(message)
    this.name = "TimeoutError"
  }
}

export class UnsupportedTransportError extends Error {
  constructor(message: string) {
    super(message)
    this.name = "UnsupportedTransportError"
  }
}
