import { expect, test } from "bun:test";

import { ApiError } from "./api";
import { authErrorMessage, errorMessage } from "./error";

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

test("auth codes map to their message and unknown codes fall back", () => {
  expect(authErrorMessage("expired_token", "Sign in failed.")).toBe("This link has expired.");
  expect(authErrorMessage("unknown", "Sign in failed.")).toBe("Sign in failed.");
  expect(authErrorMessage(null, "Sign in failed.")).toBe("Sign in failed.");
});
