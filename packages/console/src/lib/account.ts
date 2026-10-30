import { del, postJson, request } from "./api";
import type { EmailChallenge } from "./auth";

// A way to sign in to the account: a passkey, the login email or a linked
// GitHub identity.
export type SignInMethod = { id: string; method: string; label: string; created_at: string; last_used_at?: string | null };

export const listSignInMethods = () => request<{ credentials: SignInMethod[] }>("/api/account/credentials");
export const removeSignInMethod = (id: string) => del(`/api/account/credentials/${encodeURIComponent(id)}`);

export const updateDisplayName = (displayName: string) =>
  request("/api/account", { method: "PATCH", body: JSON.stringify({ display_name: displayName }) });

export const startEmailChange = (newEmail: string) =>
  postJson<{ new_email: string }, EmailChallenge>("/api/account/email-change/start", { new_email: newEmail });
export const resendEmailChange = (challengeID: string) =>
  postJson<{ challenge_id: string }, EmailChallenge>("/api/account/email-change/resend", { challenge_id: challengeID });
export const verifyEmailChange = (challengeID: string, code: string) =>
  postJson("/api/account/email-change/verify", { challenge_id: challengeID, code });

export const startGitHubLink = () => postJson<{}, { redirect_url: string }>("/api/account/providers/github/link", {});

export const revokeAllAccess = () => postJson("/api/account/access/revoke-all", {});
