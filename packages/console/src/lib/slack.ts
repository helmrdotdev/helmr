import { postJson, request } from "./api";

export type AgentSlackAddress = { projectID: string; environmentID: string; agentName: string };
export type AgentSlackPublication = {
  id: string; registration_id: string; installation_id: string | null;
  status: "setup_incomplete" | "connected" | "reauthorization_required" | "disconnected";
  credentials_configured: boolean; client_id: string | null; app_id: string | null;
  team_id: string | null; workspace_name: string | null; app_name: string | null; app_icon_url: string | null;
};
export type AgentSlackConnection = { publication: AgentSlackPublication | null; manifest?: unknown; create_app_url?: string };
export type SlackAppCredentials = { client_id: string; client_secret: string; signing_secret: string };
function publicationPath(address: AgentSlackAddress): string {
  return `/api/projects/${encodeURIComponent(address.projectID)}/environments/${encodeURIComponent(address.environmentID)}/agents/${encodeURIComponent(address.agentName)}/slack`;
}
export function getAgentSlackConnection(address: AgentSlackAddress): Promise<AgentSlackConnection> { return request(publicationPath(address)); }
export function beginAgentSlackConnection(address: AgentSlackAddress): Promise<AgentSlackConnection> { return postJson(publicationPath(address), {}); }
export function saveSlackAppCredentials(address: AgentSlackAddress, id: string, credentials: SlackAppCredentials): Promise<void> {
  return request(`${publicationPath(address)}/${encodeURIComponent(id)}/credentials`, { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify(credentials) });
}
export function authorizeAgentSlackConnection(address: AgentSlackAddress, id: string, returnTo: string): Promise<{ redirect_url: string }> {
  return postJson(`${publicationPath(address)}/${encodeURIComponent(id)}/authorize`, { return_to: returnTo });
}
export function disconnectAgentSlackConnection(address: AgentSlackAddress, id: string): Promise<void> {
  return request(`${publicationPath(address)}/${encodeURIComponent(id)}`, { method: "DELETE" });
}
export function finishSlackAuthorization(input: { state: string; code: string; error?: string }): Promise<{ installation_id: string; return_url: string }> {
  return postJson("/api/slack/installations/finish", input, { redirectOnUnauthorized: false });
}

export type SlackUserLink = { team_id: string; slack_user_id: string; linked_at: string };
export type SlackLinkWorkspace = { installation_id: string; team_id: string; workspace_name: string | null };
export type SlackIdentityProof = { identity: { team_id: string; slack_user_id: string; name: string }; confirmation: string; helmr_user_id: string; helmr_display_name: string; helmr_org_name: string; workspace_name: string | null; return_url: string };
export type SlackFirstUseLink = { organization_id: string; installation_id: string; team_id: string; slack_user_id: string; workspace_name: string | null; organization_name: string; helmr_display_name: string; return_url: string; linked: boolean };
export function getSlackFirstUseLink(link: string): Promise<SlackFirstUseLink> {
  return request(`/api/slack/user-links/first-use?${new URLSearchParams({ link })}`, { redirectOnUnauthorized: false });
}
export function startSlackFirstUseLink(link: string): Promise<{ redirect_url: string }> {
  return postJson("/api/slack/user-links/authorize", { link }, { redirectOnUnauthorized: false });
}
export function listSlackUserLinks(cursor?: string): Promise<{ links: SlackUserLink[]; next_cursor?: string }> {
  const query = new URLSearchParams({ limit: "50" }); if (cursor) query.set("cursor", cursor);
  return request(`/api/slack/user-links?${query}`);
}
export function listSlackLinkWorkspaces(cursor?: string): Promise<{ configured: boolean; workspaces: SlackLinkWorkspace[]; next_cursor?: string }> {
  const query = new URLSearchParams({ limit: "50" }); if (cursor) query.set("cursor", cursor);
  return request(`/api/slack/link-workspaces?${query}`);
}
export function startSlackUserLink(installationID: string): Promise<{ redirect_url: string }> {
  return postJson("/api/slack/user-links/authorize", { installation_id: installationID });
}
export function verifySlackUserLink(input: { state: string; code: string; error: string }): Promise<SlackIdentityProof> {
  return postJson("/api/slack/user-links/verify", input, { redirectOnUnauthorized: false });
}
export function confirmSlackUserLink(confirmation: string): Promise<void> {
  return postJson("/api/slack/user-links/confirm", { confirmation, confirmed: true }, { redirectOnUnauthorized: false });
}
export function unlinkSlackUser(link: SlackUserLink): Promise<void> {
  return request(`/api/slack/user-links/${encodeURIComponent(link.team_id)}/${encodeURIComponent(link.slack_user_id)}`, { method: "DELETE" });
}

export type SlackDeliveryPost = {
  id: string; turn_id: string | null; sequence: number; role: string; continuation_ordinal: number;
  status: string; text: string; payload_expired_at: string | null; created_at: string;
  desired_revision: number; confirmed_revision: number; publication_revision: number;
  method: string | null; attempt_id: string | null; message_ts: string | null;
  root_known: boolean; opening: boolean; stream_state: string; check_paused_at: string | null;
  disposed_at: string | null; disposed_by: string | null; error: string | null;
};
function sessionDeliveryPath(address: import("./sessions").SessionAddress): string {
  return `/api/projects/${encodeURIComponent(address.projectID)}/environments/${encodeURIComponent(address.environmentID)}/sessions/${encodeURIComponent(address.sessionID)}/slack-delivery`;
}
export function listSlackDelivery(address: import("./sessions").SessionAddress, cursor?: string): Promise<{ posts: SlackDeliveryPost[]; next_cursor?: string }> {
  const query = new URLSearchParams({ limit: "50" }); if (cursor) query.set("cursor", cursor);
  return request(`${sessionDeliveryPath(address)}?${query.toString()}`);
}
export function recoverSlackDelivery(address: import("./sessions").SessionAddress, post: SlackDeliveryPost, action: "check" | "abandon"): Promise<void> {
  return postJson(`${sessionDeliveryPath(address)}/${encodeURIComponent(post.id)}/${action}`, { attempt_id: post.attempt_id, publication_revision: post.publication_revision });
}
