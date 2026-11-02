import { postJson, request } from "./api";

// Company SSO: sign-in, first association and the account's own links.
export type SSOLink = { organization_id: string; organization_name: string; asserted_email: string; expires_at: string; signed_in_email?: string };
export type SSOBinding = { organization_id: string; organization_name: string; protocol: string; subject: string; linked_at: string };

const unauthenticated = { redirectOnUnauthorized: false } as const;

type Start = { organization?: string; next?: string; invitation_token?: string; reauthenticate?: boolean };
const START_KEY = "helmr.sso-start";

// The start is remembered for this tab so a sign-in the identity provider
// answered with a stale authentication can be repeated with a fresh one.
export function startSSO(input: Start) {
  try { sessionStorage.setItem(START_KEY, JSON.stringify({ ...input, reauthenticate: undefined })); } catch { /* the retry is a convenience */ }
  return postJson<Start, { redirect_url: string }>("/api/auth/sso/start", input, unauthenticated);
}

// restartSSO repeats this tab's last SSO start, asking the identity provider to
// authenticate again. It returns false when there is nothing to repeat.
export async function restartSSO(): Promise<boolean> {
  let saved: Start | undefined;
  try { saved = JSON.parse(sessionStorage.getItem(START_KEY) ?? "null") ?? undefined; } catch { saved = undefined; }
  if (!saved) return false;
  window.location.assign((await startSSO({ ...saved, reauthenticate: true })).redirect_url);
  return true;
}
export const finishOIDC = (input: { code: string; state: string; error?: string; error_description?: string }) =>
  postJson<typeof input, { redirect_after: string }>("/api/auth/sso/oidc/finish", input, unauthenticated);
export const finishSAML = () => postJson<Record<string, never>, { redirect_after: string }>("/api/auth/sso/saml/finish", {}, unauthenticated);
export const getSSOLink = () => request<SSOLink>("/api/auth/sso/link", unauthenticated);
export const confirmSSOLink = () => postJson<Record<string, never>, { organization_id: string; redirect_after: string }>("/api/auth/sso/link/confirm", {}, unauthenticated);
export const startSSOLink = (organizationID: string) => postJson<{ organization_id: string }, { redirect_url: string }>("/api/account/sso/link", { organization_id: organizationID });
export const listSSOBindings = () => request<{ bindings: SSOBinding[] }>("/api/account/sso/bindings");
