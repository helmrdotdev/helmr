import type { HelmrError } from "./contract"

/** Narrow coded SDK failures; native network and abort errors remain unchanged. */
export function isHelmrError(value: unknown): value is HelmrError {
  return value instanceof Error && "code" in value && typeof value.code === "string" && value.code.length > 0 &&
    (!("requestId" in value) || value.requestId === undefined || typeof value.requestId === "string")
}
