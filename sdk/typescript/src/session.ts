import type { SessionOperationOptions } from "./contract"

export function sessionOperationOptions(
  request: SessionOperationOptions = {},
): SessionOperationOptions & { readonly idempotencyKey: string } {
  return { idempotencyKey: request.idempotencyKey ?? crypto.randomUUID() }
}
