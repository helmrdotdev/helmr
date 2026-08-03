import type { JsonValue } from "./contract"
import { currentRuntimeOperations } from "./internal/runtime"

export type RunLogLevel = "debug" | "info" | "warn" | "error"
export type LogAttributes = Readonly<Record<string, JsonValue>>

function write(
  level: RunLogLevel,
  message: string,
  attributes: LogAttributes = {},
): Promise<void> {
  return currentRuntimeOperations().structuredLog(level, message, attributes)
}

export const logger = Object.freeze({
  debug(message: string, attributes?: LogAttributes): Promise<void> {
    return write("debug", message, attributes)
  },
  info(message: string, attributes?: LogAttributes): Promise<void> {
    return write("info", message, attributes)
  },
  warn(message: string, attributes?: LogAttributes): Promise<void> {
    return write("warn", message, attributes)
  },
  error(message: string, attributes?: LogAttributes): Promise<void> {
    return write("error", message, attributes)
  },
})
