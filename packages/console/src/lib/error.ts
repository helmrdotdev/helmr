const MESSAGES: Record<string, string> = {
  invalid_token: "This link is no longer valid.",
  expired_token: "This link has expired.",
  already_bootstrapped: "This Helmr instance is already set up.",
  already_accepted: "This invite has already been used.",
  wrong_account:
    "Signed in with an account that does not match this invite email.",
  access_denied: "Sign in was cancelled.",
  state_mismatch: "Sign in expired. Please try again.",
  code_exchange_failed: "Sign in expired. Please try again.",
  missing_flow_cookie: "Sign in expired. Please try again.",
  invalid_email: "Enter a valid email address.",
  email_required: "Enter your email address.",
  magic_link_email_failed: "Could not send the sign-in link. Please try again.",
  magic_link_token_missing: "This sign-in link is missing its token.",
  magic_link_token_invalid: "This sign-in link is no longer valid.",
  magic_link_token_expired: "This sign-in link has expired.",
  magic_link_token_used: "This sign-in link has already been used.",
  magic_link_not_found: "This sign-in link is no longer valid.",
  magic_link_expired: "This sign-in link has expired.",
  magic_link_already_used: "This sign-in link has already been used.",
  bootstrap_owner_email_required:
    "Initial owner setup is unavailable. Ask the operator to configure HELMR_BOOTSTRAP_OWNER_EMAIL.",
  bootstrap_owner_mismatch:
    "This account is not allowed to create the initial owner. Sign in with the configured owner email.",
  bootstrap_owner_email_unverified:
    "This GitHub App cannot verify your email address. Ask the operator to enable read access to user email addresses.",
  no_account: "No account exists for this email. Ask an owner for an invite link.",
  disabled_member: "Membership is no longer active.",
  already_member: "You are already a member of this organization.",
  unauthenticated: "Please sign in.",
};

export function errorMessage(errorKind: string | null | undefined, fallback: string): string {
  if (!errorKind) return fallback;
  return MESSAGES[errorKind] ?? fallback;
}
