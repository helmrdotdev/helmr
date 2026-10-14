import { expect, test } from "bun:test";

import { ApiError } from "./api";
import { AUTH_MESSAGES, describeFailure, errorDetails, errorMessage } from "./error";

test("API errors show the server message", () => {
  expect(errorMessage(new ApiError("conflict", "Name is taken.", 409))).toBe("Name is taken.");
});

test("a page message for the error code wins over the server message", () => {
  const error = new ApiError("forbidden", "forbidden", 403);
  expect(errorMessage(error, "Could not load.", { forbidden: "You do not have permission." })).toBe("You do not have permission.");
});

test("an empty server message falls back", () => {
  expect(errorMessage(new ApiError("internal_error", "  ", 500), "Could not load.")).toBe("Could not load.");
});

test("non-API errors never show their own message", () => {
  expect(errorMessage(new Error("TypeError: x is undefined"))).toBe("Something went wrong.");
  expect(errorMessage("boom", "Could not save.")).toBe("Could not save.");
});

test("failures use the message for their code, never the server text", () => {
  expect(describeFailure(new ApiError("invalid_token", "token expired at 12:00", 400), AUTH_MESSAGES, "Sign in failed.")).toEqual({
    message: AUTH_MESSAGES["invalid_token"],
    details: "invalid_token: token expired at 12:00",
  });
  expect(describeFailure(new ApiError("unknown", "raw server text", 500), AUTH_MESSAGES, "Sign in failed.").message).toBe("Sign in failed.");
  expect(describeFailure(new Error("boom"), AUTH_MESSAGES, "Sign in failed.")).toEqual({ message: "Sign in failed.", details: undefined });
});

test("technical details carry the code and server message of API errors only", () => {
  expect(errorDetails(new ApiError("invalid_request", "Sign-in failed", 400))).toBe("invalid_request: Sign-in failed");
  expect(errorDetails(new ApiError("internal_error", " ", 500))).toBe("internal_error");
  expect(errorDetails(new Error("TypeError: x is undefined"))).toBeUndefined();
});
