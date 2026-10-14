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

const AUTH_MESSAGES: Record<string, string> = {
  invalid_token: "This link is no longer valid.",
  expired_token: "This link has expired.",
  wrong_account:
    "Signed in with an account that does not match this invite email.",
  access_denied: "Sign in was cancelled.",
  magic_link_token_missing: "This sign-in link is missing its token.",
  already_member: "You are already a member of this organization.",
  unauthenticated: "Please sign in.",
};

// The message for a sign-in, invite or device-approval failure code.
export function authErrorMessage(code: string | null | undefined, fallback: string): string {
  if (!code) return fallback;
  return AUTH_MESSAGES[code] ?? fallback;
}
