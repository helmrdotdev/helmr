import { ApiError } from "./api";

const FALLBACK = "Something went wrong.";

// The message to show for a failed request: a page-specific message for the API
// error code when one is given, then the server's own message, then the fallback.
// Errors that are not API errors never leak their internal message.
export function errorMessage(
  error: unknown,
  fallback: string = FALLBACK,
  messages: Readonly<Record<string, string>> = {},
): string {
  if (!(error instanceof ApiError)) return fallback;
  return messages[error.code] ?? (error.message.trim() || fallback);
}

// The sentence for a failed read: the page's message for the error code, else
// its fallback. The server's own text is left to the technical details.
// Some reads refuse with `permission_required` instead of `forbidden`; both
// take the page's `forbidden` sentence.
export function loadError(error: unknown, fallback: string, messages: Readonly<Record<string, string>> = {}): string {
  const code = error instanceof ApiError ? error.code : undefined;
  if (!code) return fallback;
  return messages[code] ?? (code === "permission_required" ? messages["forbidden"] : undefined) ?? fallback;
}

export type Failure = { message: string; details: string | undefined };

// A failure as people read it: the message for its error code, else the
// fallback. The server's own text is never the message; it goes to the details.
export function describeFailure(
  error: unknown,
  messages: Readonly<Record<string, string>>,
  fallback: string,
): Failure {
  return { message: loadError(error, fallback, messages), details: errorDetails(error) };
}

// Messages for the codes sign-in, invite and device approval requests return.
export const AUTH_MESSAGES: Readonly<Record<string, string>> = {
  invalid_token: "This link is no longer valid. Request a new one.",
  wrong_account: "Signed in with an account that does not match this invite email.",
  already_member: "You are already a member of this organization.",
  unauthorized: "Your sign-in has expired. Sign in again.",
  bad_request: "The sign-in request expired or did not start in this browser. Start signing in again.",
  service_unavailable: "Sign-in is not available on this server right now. Try again later, or contact your administrator.",
  rate_limited: "Too many attempts. Wait a moment, then try again.",
};

// The technical detail of a failed request, for support: the error code and the
// server's own message. Pages show it folded away under a human message.
export function errorDetails(error: unknown): string | undefined {
  if (!(error instanceof ApiError)) return undefined;
  return [error.code, error.message.trim()].filter(Boolean).join(": ") || undefined;
}
