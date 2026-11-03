import { postJson, request } from "./api";

// Single sign-on: sign-in, first association and the account's own links.
export type SSOConfirmation = { organization_id: string; organization_name: string; asserted_email: string; expires_at: string; signed_in_email?: string };
export type SSOAccountLink = { organization_id: string; organization_name: string; protocol: string; subject: string; linked_at: string };

const unauthenticated = { redirectOnUnauthorized: false } as const;

type Start = { organization_slug?: string; next?: string; invitation_token?: string; reauthenticate?: boolean };
type Saved = { kind: "sign_in"; start: Start } | { kind: "link"; organization_id: string };
const START_KEY = "helmr.sso-start";

// Each start is remembered for this tab so a sign-in the identity provider
// answered with a stale authentication can be repeated with a fresh one.
function remember(saved: Saved) {
  try { sessionStorage.setItem(START_KEY, JSON.stringify(saved)); } catch { /* the retry is a convenience */ }
}

export function startSSO(input: Start) {
  remember({ kind: "sign_in", start: { ...input, reauthenticate: false } });
  return postJson<Start, { redirect_url: string }>("/api/auth/sso/start", input, unauthenticated);
}

export function startSSOLink(organizationID: string, reauthenticate = false) {
  remember({ kind: "link", organization_id: organizationID });
  return postJson<{ organization_id: string; reauthenticate: boolean }, { redirect_url: string }>("/api/account/sso/link", { organization_id: organizationID, reauthenticate });
}

// restartSSO repeats this tab's last SSO start, sign-in or self-link, asking
// the identity provider to authenticate again. It returns false when there is
// nothing to repeat.
export async function restartSSO(): Promise<boolean> {
  let saved: Saved | undefined;
  try { saved = JSON.parse(sessionStorage.getItem(START_KEY) ?? "null") ?? undefined; } catch { saved = undefined; }
  if (!saved) return false;
  const started = saved.kind === "link" ? await startSSOLink(saved.organization_id, true) : await startSSO({ ...saved.start, reauthenticate: true });
  window.location.assign(started.redirect_url);
  return true;
}

export const finishOIDC = (input: { code: string; state: string; error?: string; error_description?: string }) =>
  postJson<typeof input, { redirect_after: string }>("/api/auth/sso/oidc/finish", input, unauthenticated);
export const finishSAML = () => postJson<Record<string, never>, { redirect_after: string }>("/api/auth/sso/saml/finish", {}, unauthenticated);
export const getSSOConfirmation = () => request<SSOConfirmation>("/api/auth/sso/confirm", unauthenticated);
export const confirmSSOAssociation = () => postJson<Record<string, never>, { organization_id: string; redirect_after: string }>("/api/auth/sso/confirm", {}, unauthenticated);
export const listSSOLinks = () => request<{ links: SSOAccountLink[] }>("/api/account/sso/links");
