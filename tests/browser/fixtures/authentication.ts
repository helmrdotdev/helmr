// UI fixtures describe an account and its membership together for convenience;
// HTTP responses still expose the separate production API contracts.
export function isAuthenticationPath(path: string): boolean {
  return ["/api/me", "/api/auth/config", "/api/organizations", "/api/organization", "/api/operator/me"].includes(path);
}
export function authenticationResponse(path: string, value: any): unknown {
  if (!isAuthenticationPath(path)) return value;
  const member = { id: value.membership_id ?? "fixture-member", user_id: value.user_id, display_name: value.display_name, is_owner: value.is_owner ?? false, permissions: value.permissions ?? [], permission_scopes: value.permission_scopes ?? [] };
  const organization = value.org_id ? { id: value.org_id, name: value.org_name ?? "Organization", slug: "organization", membership_id: member.id, authentication_required: "" } : undefined;
  if (path === "/api/me") return { user_id: value.user_id, display_name: value.display_name, primary_email: value.primary_email ?? "fixture@example.test", profile_image_url: value.profile_image_url ?? "" };
  if (path === "/api/auth/config") return { deployment_mode: "cloud", methods: ["email", "passkey", "github"], email_delivery: value.email_delivery ?? true, app_origin: value.app_origin ?? "", signup_enabled: true, organization_creation_enabled: true, setup_required: false };
  if (path === "/api/organizations") return { organizations: organization ? [organization] : [] };
  if (path === "/api/operator/me") return { id: "fixture-operator", expires_at: new Date(Date.now()+43200000).toISOString() };
  return { ...organization, current_member: member, authentication: { method: "passkey", authenticated_at: new Date().toISOString(), reauthenticate_at: null }, onboarding: { project_required: value.project_required ?? false } };
}
