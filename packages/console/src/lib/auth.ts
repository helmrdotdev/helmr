import { ApiError, postJson, request, selectedOrganization, setOrganization } from "./api";

export type Account = {user_id:string;display_name:string;profile_image_url:string;primary_email:string};
export type OrganizationMembership = {id:string;name:string;slug:string;membership_id:string;authentication_required:string};
export type AuthConfig = {deployment_mode:string;methods:string[];email_delivery:boolean;app_origin:string;signup_enabled:boolean;organization_creation_enabled:boolean;single_organization?:OrganizationMembership;setup_required:boolean};
export type OrganizationContext = {id:string;name:string;slug:string;current_member:{id:string;user_id:string;display_name:string;is_owner:boolean;permissions:string[];permission_scopes:{project:string;environment:string;agent?:string;permissions:string[]}[]};authentication:{authenticated_at:string};onboarding:{project_required:boolean}};
// Console projection; account and organization have distinct API/proof sources.
export type Me = Account & {
 org_id?:string;org_name?:string;app_origin:string;membership_id?:string;is_owner:boolean;
 permissions:string[];permission_scopes:OrganizationContext["current_member"]["permission_scopes"];
 organization_access_denied?:boolean;organization_required:boolean;project_required:boolean;access_required:boolean;
 email_delivery:boolean;organizations:OrganizationMembership[];organization_selection_required:boolean;setup_required:boolean;
};
export const getAccount = () => request<Account>("/api/me",{redirectOnUnauthorized:false});
export const getAuthConfig = () => request<AuthConfig>("/api/auth/config",{redirectOnUnauthorized:false});
export const getOrganizations = () => request<{organizations:OrganizationMembership[]}>("/api/organizations",{redirectOnUnauthorized:false});
export async function getMe():Promise<Me>{
 const account=await getAccount();
 const [config,memberships]=await Promise.all([getAuthConfig(),getOrganizations()]);
 let org=selectedOrganization();
 if(!org){org=config.single_organization?.id??(memberships.organizations.length===1?memberships.organizations[0]!.id:"");if(org)setOrganization(org);}
 let context:OrganizationContext|undefined;let denied=false;
 if(org&&memberships.organizations.some(m=>m.id===org)) {
  try{context=await request<OrganizationContext>("/api/organization",{redirectOnUnauthorized:false});}
  catch(error){if(error instanceof ApiError&&error.code==="access_denied")denied=true;else throw error;}
 }
 return {...account,organization_access_denied:denied,app_origin:config.app_origin,email_delivery:config.email_delivery,organizations:memberships.organizations,
 ...(context?{org_id:context.id,org_name:context.name,membership_id:context.current_member.id}:{}),
 is_owner:context?.current_member.is_owner??false,permissions:context?.current_member.permissions??[],permission_scopes:context?.current_member.permission_scopes??[],
 organization_required:!context&&memberships.organizations.length===0&&config.organization_creation_enabled,
 access_required:!context&&memberships.organizations.length===0&&!config.organization_creation_enabled,
 organization_selection_required:!denied&&!context&&memberships.organizations.length>0,
 setup_required:config.setup_required,project_required:context?.onboarding.project_required??false};
}

export function hasPermission(me: Me | undefined, permission: string, scope?: {projectID:string;environmentID:string;agent?:string}): boolean {
 if(me?.permissions.includes(permission)) return true;
 return !!scope && (me?.permission_scopes??[]).some(g=>g.project===scope.projectID && g.environment===scope.environmentID && (!g.agent || g.agent===scope.agent) && g.permissions.includes(permission));
}

export type AuthRequirement = "session" | "organization" | "project";

export function onboardingRedirectPath(me: Me, requirement: AuthRequirement = "project"): string | null {
  if (requirement === "session") return null;
  if(me.setup_required)return "/setup";
  if(me.organization_selection_required)return "/organizations";
  if (me.access_required) return "/access-required";
  if (me.organization_required) return "/organizations/new";
  if (requirement === "project" && me.project_required) return "/projects/new";
  return null;
}

export type EmailChallenge={challenge_id:string;expires_at:string;resend_after:string};
export const startEmailLogin=(input:{email:string;next?:string;invitation_token?:string})=>postJson<typeof input,EmailChallenge>("/api/auth/email/start",input,{redirectOnUnauthorized:false});
export const resendEmailCode=(id:string)=>postJson<{},EmailChallenge>(`/api/auth/email/${id}/resend`,{},{redirectOnUnauthorized:false});
export const verifyEmailCode=(id:string,code:string)=>postJson<{code:string},{redirect_after:string}>(`/api/auth/email/${id}/verify`,{code},{redirectOnUnauthorized:false});

export async function startGitHubLogin(next?: string): Promise<{ redirect_url: string }> {
  return postJson<{ next?: string }, { redirect_url: string }>(
    "/api/auth/github/start",
    next ? { next } : {},
    { redirectOnUnauthorized: false },
  );
}

export async function startGitHubInvite(token: string): Promise<{ redirect_url: string }> {
  return postJson<{ token: string }, { redirect_url: string }>(
    "/api/auth/github/invite/start",
    { token },
    { redirectOnUnauthorized: false },
  );
}

export async function finishGitHubAuth(input: {
  code: string;
  state: string;
  error?: string;
  error_description?: string;
}): Promise<{ redirect_after: string }> {
  return postJson<
    {
      code: string;
      state: string;
      error?: string;
      error_description?: string;
    },
    { redirect_after: string }
  >("/api/auth/github/finish", input, { redirectOnUnauthorized: false });
}

export async function logout(): Promise<void> {
  await postJson<Record<string, never>, void>("/api/auth/logout", {});
  window.location.href = "/login";
}

export type DeviceCodeStatus = {
 consent?:{account_name:string;org_name:string;proof:string};
  status: "pending" | "approved" | "denied" | "consumed" | "expired";
  expires_at?: string;
};

export async function getDeviceCodeStatus(userCode: string): Promise<DeviceCodeStatus> {
  return request<DeviceCodeStatus>(
    `/api/auth/device/status?user_code=${encodeURIComponent(userCode)}`,
  );
}

export async function approveDeviceCode(userCode: string, proof: string): Promise<DeviceCodeStatus> {
  return postJson<{ user_code: string }, DeviceCodeStatus>("/api/auth/device/approve", { user_code: userCode }, { headers: { "X-Helmr-Device-Consent": proof } });
}

export async function denyDeviceCode(userCode: string, proof: string): Promise<DeviceCodeStatus> {
  return postJson<{ user_code: string }, DeviceCodeStatus>("/api/auth/device/deny", { user_code: userCode }, { headers: { "X-Helmr-Device-Consent": proof } });
}
