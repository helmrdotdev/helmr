import { del, request } from "./api";

export type PermissionDefinition = { resource_type: string; id: string; name: string; description: string; scope_types: ("organization" | "environment" | "agent")[]; assignment_levels: ("organization" | "environment")[]; api_key_assignable: boolean };
export type PermissionRole = { assignment_levels: ("organization" | "environment")[]; api_key_assignable: boolean; id: string; name: string; description: string; builtin_key: string | null; permissions: string[]; grant_count: number; group_count: number; updated_at: string; revision: number };
export type RoleInput = Pick<PermissionRole, "name" | "description" | "permissions">;
export type PermissionGroup = { id: string; name: string; member_ids: string[]; revision: number };
export type GrantTarget = { resource_type:string; mode:string; agent_ids?:string[]; agent_keys?:string[]; permissions:string[] };
export type RoleGrant = { api_key_id?: string; api_key_name?: string; id:string; revision:number; subject_kind:"member"|"group"|"api_key"|"all_members"; member_id?:string; member_name?:string; group_id:string|null; group_name?:string; role_id:string; project_id:string; project:string; environment_id:string; environment:string; target_kind:"organization"|"environment"; agents_mode:"none"|"selected"|"all"; agent_ids:string[]; agent_keys:string[]; targets:GrantTarget[]; role_name:string; project_name:string; environment_name:string; permissions:string[] };
export type GrantInput = { api_key_id?: string|undefined; subject_kind:"member"|"group"|"api_key"|"all_members"; member_id?:string|undefined; group_id:string|null; role_id:string; environment_id:string; target_kind:"organization"|"environment"; agents_mode:"none"|"selected"|"all"; agent_ids:string[]; expected_role_revision:number };
export type AccessTarget = { environment_id: string; environment: string; name: string; agents: { id: string; key: string }[] };
export async function listRoles():Promise<{roles:PermissionRole[];catalog:PermissionDefinition[]}> {
 const roles:PermissionRole[]=[];let catalog:PermissionDefinition[]=[];let cursor="";
 do {const page=await request<{roles:PermissionRole[];catalog:PermissionDefinition[];next_cursor?:string}>(`/api/roles?limit=100&cursor=${encodeURIComponent(cursor)}`);roles.push(...page.roles);catalog=page.catalog;cursor=page.next_cursor??"";}while(cursor);
 return {roles,catalog};
}
export async function listGroups():Promise<{groups:PermissionGroup[]}> {
 const groups:PermissionGroup[]=[];let cursor="";
 do {const page=await request<{groups:PermissionGroup[];next_cursor?:string}>(`/api/groups?limit=100&cursor=${encodeURIComponent(cursor)}`);groups.push(...page.groups);cursor=page.next_cursor??"";}while(cursor);
 return {groups};
}
export async function listGrants(): Promise<{grants: RoleGrant[]}> {
 const grants:RoleGrant[]=[];let cursor="";
 do { const page=await request<{grants:RoleGrant[];next_cursor?:string}>(`/api/role-grants?limit=100&cursor=${encodeURIComponent(cursor)}`);grants.push(...page.grants);cursor=page.next_cursor??""; }while(cursor);
 return {grants};
}
export const listAccessTargets = (project: string) => request<{ project_id: string; targets: AccessTarget[] }>(`/api/projects/${encodeURIComponent(project)}/access-targets`);
export const saveRole = (input: RoleInput, id?: string, revision?: number) => request<PermissionRole>(`/api/roles${id ? `/${encodeURIComponent(id)}` : ""}`, { method: id ? "PATCH" : "POST", body: JSON.stringify(input), headers: id ? { "If-Match": `"${revision}"` } : {} });
export const deleteRole = (id: string) => del(`/api/roles/${encodeURIComponent(id)}`);
export const saveGroup = (name: string, id?: string, revision?: number) => request<PermissionGroup>(`/api/groups${id ? `/${encodeURIComponent(id)}` : ""}`, { method: id ? "PATCH" : "POST", body: JSON.stringify({ name }), headers: id ? { "If-Match": `"${revision}"` } : {} });
export const deleteGroup = (id: string) => del(`/api/groups/${encodeURIComponent(id)}`);
export const setGroupMember = (group: string, user: string, add: boolean) => request(`/api/groups/${encodeURIComponent(group)}/members/${encodeURIComponent(user)}`, { method: add ? "PUT" : "DELETE" });
export async function assignRole(project:string,input:GrantInput,existing?:Pick<RoleGrant,"id"|"revision">) {
 const projectID=input.target_kind === "organization" ? undefined : (await listAccessTargets(project)).project_id;
 const subject=input.subject_kind === "member" ? {type:"member",member_id:input.member_id} : input.subject_kind === "group" ? {type:"group",group_id:input.group_id} : input.subject_kind === "api_key" ? {type:"api_key",api_key_id:input.api_key_id} : {type:"all_members"};
 return request(`/api/role-grants${existing ? `/${encodeURIComponent(existing.id)}` : ""}`,{method:existing ? "PATCH" : "POST",headers:existing ? {"If-Match":`"${existing.revision}"`} : {},body:JSON.stringify({role_id:input.role_id,subject,project_id:projectID,environment_id:input.target_kind === "organization" ? undefined : input.environment_id,agents_mode:input.agents_mode,agent_ids:input.agent_ids,expected_role_revision:input.expected_role_revision})});
}
export const removeGrant=(id:string,revision:number)=>request(`/api/role-grants/${encodeURIComponent(id)}`,{method:"DELETE",headers:{"If-Match":`"${revision}"`}});
// Agents come first: they are what most roles are about.
const RESOURCE_ORDER = ["agents", "agent_connections", "sessions", "triggers", "deployments", "templates", "computers", "commands", "secrets", "api_keys", "projects", "environments", "members", "invitations", "groups", "roles", "assignments"];
export const resourceRank = (resource: string) => { const i = RESOURCE_ORDER.indexOf(resource); return i < 0 ? RESOURCE_ORDER.length : i; };
export const resourceLabel=(type:string)=>({agents:"Agents",sessions:"Sessions",agent_connections:"Agent connections",triggers:"Triggers",templates:"Templates",projects:"Projects",environments:"Environments",members:"Members",invitations:"Invitations",groups:"Groups",roles:"Roles",assignments:"Assignments",deployments:"Deployments",computers:"Computers",commands:"Commands",secrets:"Secrets",api_keys:"API keys",audit_events:"Audit events",organization:"Organization"}[type] ?? type);
export const targetSummary=(grant:RoleGrant)=>grant.targets.map(t=>`${resourceLabel(t.resource_type)}: ${t.mode === "selected" ? t.agent_keys?.join(", ") || "No remaining agent targets" : t.mode === "all" ? "All current and future agents" : t.mode === "organization" ? "Organization" : "Entire environment"}`).join(" · ");
// The route ID of the built-in all-members audience on the Groups pages.
export const ALL_MEMBERS = "all-members";
export const memberHref = (id: string) => `/settings/members/${encodeURIComponent(id)}`;
export const groupHref = (id: string) => `/settings/groups/${encodeURIComponent(id)}`;
export const roleHref = (id: string) => `/settings/roles/${encodeURIComponent(id)}`;

export const permissionName = (id: string, catalog: readonly PermissionDefinition[]) => catalog.find(c => c.id === id)?.name ?? id;
export const permissionNames = (ids: readonly string[], catalog: readonly PermissionDefinition[]) => ids.map(id => permissionName(id, catalog)).join(" · ");
export const permissionResource = (id: string, catalog: readonly PermissionDefinition[]) => catalog.find(c => c.id === id)?.resource_type ?? "";
export const permissionResources = (ids: readonly string[], catalog: readonly PermissionDefinition[]) => [...new Set(ids.map(id => permissionResource(id, catalog)).filter(Boolean))];
// The server supplies each action's eligible assignment levels. This intersection
// is a draft preview; save/assignment validation remains server-authoritative.
export const roleAssignmentLevels = (permissions: readonly string[], catalog: readonly PermissionDefinition[]) =>
  (["organization", "environment"] as const).filter(level => permissions.length > 0 && permissions.every(id => catalog.find(c => c.id === id)?.assignment_levels.includes(level)));

// The recipient of an assignment, as its pages name it.
export function grantRecipientName(grant: RoleGrant): string {
  if (grant.subject_kind === "all_members") return "All members";
  if (grant.subject_kind === "api_key") return grant.api_key_name || grant.api_key_id || "API key";
  if (grant.subject_kind === "member") return grant.member_name || grant.member_id || "Member";
  return grant.group_name || grant.group_id || "Group";
}

// The agents an assignment selects: "All agents", their keys, or "" without agent permissions.
export function grantAgents(grant: RoleGrant): string {
  if (grant.agents_mode === "all") return "All agents";
  if (grant.agents_mode === "selected") return grant.agent_keys.join(", ") || "No agents";
  return "";
}

export const grantWhere = (grant: RoleGrant) => grant.target_kind === "organization" ? "Organization" : grant.environment_name;

export function grantCoversAgent(grant: RoleGrant, environmentID: string, agentID: string): boolean {
  return grant.target_kind === "environment" && grant.environment_id === environmentID && (grant.agents_mode === "all" || grant.agent_ids.includes(agentID));
}

// A group's assignments, or all members' for the ALL_MEMBERS route ID.
export function subjectGrants(grants: readonly RoleGrant[], group: string): RoleGrant[] {
  return grants.filter((g) => group === ALL_MEMBERS ? g.subject_kind === "all_members" : g.subject_kind === "group" && g.group_id === group);
}
export function memberGrantSources(grants: readonly RoleGrant[], groups: readonly PermissionGroup[], user: string): RoleGrant[] {
  const memberships = new Set(groups.filter((g) => g.member_ids.includes(user)).map((g) => g.id));
  return grants.filter((g) => g.subject_kind === "all_members" || (g.subject_kind === "member" && g.member_id === user) || (g.group_id !== null && memberships.has(g.group_id)));
}

export type MemberPermissionSource = {kind: "role_grant"; subject_kind:"member"|"group"|"all_members"; member_id?:string; target_kind?:string; agent?:string; agent_id?:string; grant_id?: string; group_id?:string; group_name?:string; role_name?:string; permissions:string[]};
export type MemberPermissions = {user_id:string;membership_id:string;is_owner:boolean;status:string;notice:string;scopes:{type:"organization"|"environment"|"agent";project:string;environment:string;agent?:string;permissions:string[];sources:MemberPermissionSource[]}[]};
export const getMemberPermissions=(user:string)=>request<MemberPermissions>(`/api/members/${encodeURIComponent(user)}/permissions`);

export const permissionHasScope = (id: string, scope: "organization" | "environment" | "agent", catalog: readonly PermissionDefinition[]) => catalog.find(c => c.id === id)?.scope_types.includes(scope) ?? false;

export const getPermissionCatalog = () => request<{catalog: PermissionDefinition[]}>("/api/permission-catalog");
