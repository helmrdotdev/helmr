import type { JsonValue, RunHandle } from "../contract"
import { resourceID } from "./id"

export function createRunHandle<TOutput extends JsonValue = JsonValue>(
  id: unknown,
): RunHandle<TOutput> {
  return Object.freeze({
    id: resourceID(id, "Run ID"),
  }) as RunHandle<TOutput>
}

export function runHandleID(value: string | RunHandle): string {
  return resourceID(typeof value === "string" ? value : value.id, "Run ID")
}
