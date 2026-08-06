import { describe, expect, test } from "bun:test"

import { installRuntimeOperations } from "./internal"
import { logger, metadata } from "./index"

describe("Run observability", () => {
  test("delegates metadata mutations and all structured log levels", async () => {
    const calls: unknown[] = []
    const uninstall = installRuntimeOperations({
      waitFor: async () => {},
      waitUntil: async () => {},
      actorInputSend: async () => ({
        id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc34",
        sequence: 1,
        data: null,
        source: { type: "external" },
        createdAt: "2026-07-24T11:50:00Z",
      }),
      tokenCreate: async () => {
        throw new Error("unused")
      },
      tokenWait: async () => null,
      metadataSet: async (key, value) => {
        calls.push({ operation: "set", key, value })
      },
      metadataPatch: async (values) => {
        calls.push({ operation: "patch", values })
      },
      metadataIncrement: async (key, amount) => {
        calls.push({ operation: "increment", key, amount })
      },
      structuredLog: async (level, message, attributes) => {
        calls.push({ operation: "log", level, message, attributes })
      },
    })
    try {
      await metadata.set("phase", "review")
      await metadata.patch({ approved: true })
      await metadata.increment("attempts")
      await logger.debug("debug")
      await logger.info("info", { step: 1 })
      await logger.warn("warn")
      await logger.error("error", { retryable: false })
    } finally {
      uninstall()
    }

    expect(calls).toEqual([
      { operation: "set", key: "phase", value: "review" },
      { operation: "patch", values: { approved: true } },
      { operation: "increment", key: "attempts", amount: 1 },
      { operation: "log", level: "debug", message: "debug", attributes: {} },
      {
        operation: "log",
        level: "info",
        message: "info",
        attributes: { step: 1 },
      },
      { operation: "log", level: "warn", message: "warn", attributes: {} },
      {
        operation: "log",
        level: "error",
        message: "error",
        attributes: { retryable: false },
      },
    ])
  })
})
