import { expect, test } from "bun:test";
import { ApiError } from "../../lib/api";
import { slackFailure, slackNotConfigured } from "./failure";

test("Slack failures read as people-facing messages with the server text in the details", () => {
  const unavailable = new ApiError("service_unavailable", "service is unavailable", 503);
  expect(slackNotConfigured(unavailable)).toBe(true);
  expect(slackFailure(unavailable, "fallback")).toEqual({ message: "Slack is not set up in Helmr.", details: "service_unavailable: service is unavailable" });
  expect(slackFailure(new ApiError("conflict", "the Agent's Slack connection is unavailable", 409), "fallback").message).toBe("The Slack connection changed since this page loaded. Reload to see its current state.");
  expect(slackFailure(new ApiError("internal_error", "the Slack operation could not be completed", 500), "The connection could not be updated.").message).toBe("The connection could not be updated.");
  expect(slackFailure(new Error("network"), "fallback")).toEqual({ message: "fallback", details: undefined });
  expect(slackNotConfigured(new ApiError("conflict", "x", 409))).toBe(false);
  expect(slackFailure(new ApiError("conflict", "x", 409), "fallback", { conflict: "Delivery changed." }).message).toBe("Delivery changed.");
});
