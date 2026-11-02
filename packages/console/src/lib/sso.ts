import { postJson, request } from "./api";

// Company SSO: sign-in, first association and the account's own links.
export type SSOLink = { organization_id: string; organization_name: string; asserted_email: string; expires_at: string; signed_in_email?: string };
export type SSOBinding = { organization_id: string; organization_name: string; protocol: string; subject: string; linked_at: string };

const unauthenticated = { redirectOnUnauthorized: false } as const;

export const startSSO = (input: { organization?: string; next?: string; invitation_token?: string; reauthenticate?: boolean }) =>
  postJson<typeof input, { redirect_url: string }>("/api/auth/sso/start", input, unauthenticated);
export const finishOIDC = (input: { code: string; state: string; error?: string; error_description?: string }) =>
  postJson<typeof input, { redirect_after: string }>("/api/auth/sso/oidc/finish", input, unauthenticated);
export const finishSAML = () => postJson<Record<string, never>, { redirect_after: string }>("/api/auth/sso/saml/finish", {}, unauthenticated);
export const getSSOLink = () => request<SSOLink>("/api/auth/sso/link", unauthenticated);
export const confirmSSOLink = () => postJson<Record<string, never>, { organization_id: string; redirect_after: string }>("/api/auth/sso/link/confirm", {}, unauthenticated);
export const startSSOLink = (organizationID: string) => postJson<{ organization_id: string }, { redirect_url: string }>("/api/account/sso/link", { organization_id: organizationID });
export const listSSOBindings = () => request<{ bindings: SSOBinding[] }>("/api/account/sso/bindings");
