const MESSAGES: Record<string, string> = {
  invalid_token: "This link is no longer valid.",
  expired_token: "This link has expired.",
  wrong_account:
    "Signed in with an account that does not match this invite email.",
  access_denied: "Sign in was cancelled.",
  magic_link_token_missing: "This sign-in link is missing its token.",
  no_account: "No account exists for this email. Ask an owner for an invite link.",
  disabled_member: "Membership is no longer active.",
  already_member: "You are already a member of this organization.",
  unauthenticated: "Please sign in.",
};

export function errorMessage(code: string | null | undefined, fallback: string): string {
  if (!code) return fallback;
  return MESSAGES[code] ?? fallback;
}
