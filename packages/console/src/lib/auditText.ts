import type { AdminAuditCheck as AuditCheck, AnyAuditEvent as AuditEvent } from "./audit";
import { formatID as shortID } from "../ui/id";

// Labels for the action categories the server assigns.
export const CATEGORY_LABELS: Record<string, string> = {
  access: "Access",
  sign_in: "Sign-in",
  agents: "Agents and sessions",
  slack: "Slack",
  computers: "Computers and commands",
  secrets: "Secrets and API keys",
  deployments: "Projects and deployments",
  infrastructure: "Infrastructure",
  audit: "Audit",
};

// Words for the resource an action names, as it reads in a sentence.
const NOUNS: Record<string, string> = {
  access: "access", member: "member", invitation: "invitation", role: "role", role_grant: "role assignment",
  permission_group: "group", platform_admin: "Helmr administrator", organization: "organization",
  identity: "sign-in", agent: "agent", session: "session", turn: "turn", input: "question", message: "message",
  trigger: "trigger", mcp: "tool connection", runtime: "runtime operation", slack_app: "Slack app",
  slack_installation: "Slack workspace", slack_connection: "Slack connection", slack_user: "Slack user link",
  computer: "computer", command: "command", template: "template", secret: "secret", api_key: "API key",
  deployment: "deployment", project: "project", environment: "environment", region: "region",
  worker_group: "worker group", worker_pool: "worker pool", worker_host: "worker host", worker_scale_in: "worker scale-in",
  audit: "audit events",
};

// Past-tense verbs for the change an action names.
const VERBS: Record<string, string> = {
  created: "Created", deleted: "Deleted", updated: "Updated", granted: "Granted", removed: "Removed",
  revoked: "Revoked", rotated: "Rotated", issued: "Issued", promoted: "Promoted", published: "Published",
  restored: "Restored", accepted: "Accepted", expired: "Expired", admitted: "Accepted", rejected: "Rejected",
  cancelled: "Cancelled", closed: "Closed", completed: "Completed", failed: "Failed", refused: "Refused",
  answered: "Answered", requested: "Requested", delivered: "Delivered", recovered: "Recovered", refreshed: "Refreshed",
  connected: "Connected", disconnected: "Disconnected", linked: "Linked", unlinked: "Unlinked", searched: "Searched",
  member_added: "Added a member to", member_removed: "Removed a member from", ownership_changed: "Changed ownership of",
  retention_changed: "Changed retention of", content_read: "Read the content of",
  output_read: "Read the output of", wake_requested: "Requested waking", hibernate_requested: "Requested hibernating", wake_settled: "Settled a wake request for", hibernate_settled: "Settled a hibernate request for",
  activated: "Activated", paused: "Paused", drain_started: "Started draining", disabled: "Disabled", marked_lost: "Marked as lost",
  cancel_requested: "Requested cancelling", close_requested: "Requested closing", interrupt_requested: "Requested interrupting", resume_requested: "Requested resuming", refresh_requested: "Requested refreshing", checkpoint_published: "Saved a checkpoint of", restore_completed: "Restored",
  exposed: "Made available", sign_in: "Signed in",
};

// Actions whose generic wording reads badly.
const OVERRIDES: Record<string, string> = {
  "platform_admin.granted": "Granted Helmr administrator access",
  "session.hold_created": "Placed a hold on a session",
  "session.hold_released": "Released a hold on a session",
  "identity.sign_in": "Signed in",
  "identity.session_revoked": "Ended a sign-in session",
  "identity.magic_link_requested": "Asked for a sign-in link",
  "identity.device_started": "Started signing in a device",
  "identity.device_authorized": "Approved a device sign-in",
  "identity.device_denied": "Denied a device sign-in",
  "audit.searched": "Searched audit events",
  "audit.retention_observed": "Observed audit retention",
  "audit.purge_completed": "Removed expired audit events",
  "audit.restore_recorded": "Recorded an audit restore",
  "secret.exposed": "Made a secret available to a computer",
  "input.requested": "Asked a person a question",
  "input.answered": "Answered a question",
  "session.content_read": "Read session content",
  "command.output_read": "Read command output",
  "access.recovered": "Recovered access administration",
};

// What an action does, as a short phrase: "Granted role", "Read session content".
export function actionLabel(action: string): string {
  if (OVERRIDES[action]) return OVERRIDES[action];
  const [resource = action, verb = ""] = action.split(".", 2);
  const noun = NOUNS[resource] ?? resource.replaceAll("_", " ");
  const word = VERBS[verb];
  return word ? `${word} ${noun}` : `${noun} ${verb.replaceAll("_", " ")}`.replace(/^./, (c) => c.toUpperCase());
}

export type ActorKind = "operator" | "member" | "api_key" | "session" | "worker_host" | "helmr" | "platform_service" | "unidentified";

// Who acted: a person, an API key, an agent's session, a worker host or Helmr
// itself, with the label recorded at the time.
export function actorOf(event: AuditEvent): { kind: ActorKind; name: string; detail?: string | undefined; id?: string } {
  const label = event.actor_identity?.display_name;
  if (event.actor_operator_id) return { kind: "operator", name: label || `Operator ${shortID(event.actor_operator_id)}`, id: event.actor_operator_id };
  if (event.actor_user_id) return { kind: "member", name: label || `Member ${shortID(event.actor_user_id)}`, detail: event.actor_identity?.email, id: event.actor_user_id };
  if (event.actor_api_key_id) return { kind: "api_key", name: label || `API key ${shortID(event.actor_api_key_id)}`, detail: "API key", id: event.actor_api_key_id };
  if (event.actor_session_id) return { kind: "session", name: label || `Session ${shortID(event.actor_session_id)}`, detail: "Agent session", id: event.actor_session_id };
  if ("actor_kind" in event && event.actor_kind === "platform_service") return { kind: "platform_service", name: "Platform service" };
  if ("actor_worker_host_id" in event && event.actor_worker_host_id) return { kind: "worker_host", name: `Worker host ${shortID(event.actor_worker_host_id)}`, id: event.actor_worker_host_id };
  if ("actor_component" in event && event.actor_component) return { kind: "helmr", name: "Helmr", detail: event.actor_component.replaceAll("_", " ") };
  return { kind: "unidentified", name: "Unidentified" };
}

// Names the viewer can resolve, by ID; anything missing shows its kind and a
// short ID.
export type AuditNames = { get: (id: string) => string | undefined };

function detailName(event: AuditEvent): string | undefined {
  const details = event.details as Record<string, unknown> | null;
  if (!details || typeof details !== "object") return undefined;
  for (const key of ["name", "display_name"]) if (typeof details[key] === "string" && details[key]) return details[key] as string;
  const after = details["after"] as Record<string, unknown> | undefined;
  if (after && typeof after["name"] === "string") return after["name"];
  return undefined;
}

// What kind of object an event acted on: "Role assignment", "Session".
export function targetNoun(kind: string): string {
  return (NOUNS[kind] ?? kind.replaceAll("_", " ")).replace(/^./, (c) => c.toUpperCase());
}

// The object an event acted on, by name where known.
export function targetLabel(event: AuditEvent, names: AuditNames): string | undefined {
  const id = event.target_id ?? event.target_user_id;
  const noun = NOUNS[event.target_kind] ?? event.target_kind.replaceAll("_", " ");
  if (!id) return event.target_kind === "audit" ? undefined : noun;
  const name = names.get(id) ?? detailName(event);
  return name ?? `${noun} ${shortID(id)}`;
}

const REFUSED: Record<string, string> = { denied: "Refused", failed: "Failed" };

// The event as one line: "Granted role · Release manager", or "Refused · Read
// session content · session 0000…0702" when Helmr said no.
export function eventSummary(event: AuditEvent, names: AuditNames): string {
  const parts = [actionLabel(event.action)];
  const target = targetLabel(event, names);
  if (target && !parts[0]!.toLowerCase().endsWith(target.toLowerCase())) parts.push(target);
  const refused = REFUSED[event.outcome];
  return refused ? `${refused}: ${parts.join(" · ")}` : parts.join(" · ");
}

// One labelled value in an event's details. `id` is the recorded identifier
// shown beside a resolved name; `sub` is a muted qualifier.
export type AuditField = { label: string; value: string; id?: string | undefined; sub?: string | undefined; href?: string | undefined; code?: boolean; tone?: "passed" | "failed"; muted?: boolean };

const REASONS: Record<string, string> = {
  no_matching_grant: "No role assignment gives this permission here",
  organization_mismatch: "The request was for a different organization",
  key_permission_not_supported: "API keys cannot have this permission",
  key_scope_mismatch: "Outside the API key's environment",
  unsupported_permission: "Not a permission Helmr knows",
  generation_changed: "The session's runtime had been replaced",
  active_membership_not_found: "No active membership in the organization",
  current_key_not_found: "The API key no longer exists",
  credential_version_changed: "The API key changed after the request was signed",
};

const CHECK_TITLES: Record<string, string> = {
  permission: "Permission",
  membership_attribute: "Organization owner",
  platform_operator: "Platform operator",
  user_attribute: "Helmr administrator",
  runtime_binding: "Current runtime",
  principal_currency: "Current credential",
  host_assignment: "Worker host assignment",
};

const sentence = (text: string) => text.replaceAll("_", " ").replace(/^./, (c) => c.toUpperCase());

// What a check looked at, as a short heading.
export function checkTitle(check: AuditCheck): string {
  return CHECK_TITLES[check.type] ?? sentence(check.type);
}

// Where a permission applied: "Agent demo-triage in Production", "The whole organization".
export function scopeText(scope: AuditCheck["scope"], names: AuditNames): string {
  if (!scope) return "Not recorded";
  const environment = scope.environment_id ? names.get(scope.environment_id) ?? `environment ${shortID(scope.environment_id)}` : undefined;
  if (scope.agent_id) return `Agent ${names.get(scope.agent_id) ?? scope.agent_id}${environment ? ` in ${environment}` : ""}`;
  if (environment) return environment.replace(/^./, (c) => c.toUpperCase());
  if (scope.project_id) return `Project ${names.get(scope.project_id) ?? shortID(scope.project_id)}`;
  return "The whole organization";
}

const result = (check: AuditCheck): AuditField => ({ label: "Result", value: check.result === "passed" ? "Passed" : "Failed", tone: check.result === "passed" ? "passed" : "failed" });
const reason = (check: AuditCheck): AuditField[] => (check.reason ? [{ label: "Reason", value: REASONS[check.reason] ?? sentence(check.reason), sub: check.reason }] : []);
// A known name with its short ID beside it, or the kind and short ID alone.
const named = (id: string, names: AuditNames, noun: string): Pick<AuditField, "value" | "id"> => {
  const name = names.get(id);
  return name ? { value: name, id: shortID(id) } : { value: `${noun} ${shortID(id)}` };
};
const person = (label: string, id: string | undefined, names: AuditNames, noun: string): AuditField[] => (id ? [{ label, ...named(id, names, noun) }] : []);

// A check's recorded fields under plain labels. A check type this Console does
// not know shows its plain values under their own names; the JSON tab has the rest.
export function checkFields(check: AuditCheck, names: AuditNames, links: { session: (id: string) => string; role: (id: string) => string }): AuditField[] {
  switch (check.type) {
    case "permission": {
      const evidence = check.evidence;
      const through: AuditField[] = !evidence ? [] : evidence.group_id
        ? [{ label: "Through", ...named(evidence.group_id, names, "group"), value: `Group ${names.get(evidence.group_id) ?? shortID(evidence.group_id)}` }]
        : [{ label: "Through", value: evidence.subject === "api_key" ? "The API key's own role" : evidence.subject === "all_members" ? "Everyone in the organization" : "A direct assignment" }];
      return [
        { label: "Needed", value: check.permission ?? "Not recorded", code: true },
        ...person("For", check.subject_user_id, names, "member"),
        ...person("For", check.subject_api_key_id, names, "API key"),
        { label: "Where", value: scopeText(check.scope, names) },
        result(check),
        ...reason(check),
        ...through,
        ...(evidence?.role_id ? [{ label: "Role", ...named(evidence.role_id, names, "role"), sub: evidence.role_revision ? `revision ${evidence.role_revision}` : undefined, href: links.role(evidence.role_id) }] : []),
        ...(evidence?.assignment_id ? [{ label: "Assignment", value: shortID(evidence.assignment_id), code: true, sub: evidence.assignment_revision ? `revision ${evidence.assignment_revision}` : undefined }] : []),
      ];
    }
    case "membership_attribute":
      return [
        ...person("Member", check.member_id, names, "member"),
        { label: "Expected", value: check.expected ? "The organization owner" : "Not the owner" },
        { label: "Observed", value: check.observed ? "The organization owner" : "Not the owner" },
        result(check),
      ];
    case "user_attribute":
      return [
        ...person("User", check.user_id, names, "user"),
        { label: "Expected", value: check.expected ? "Helmr administrator" : "Not a Helmr administrator" },
        { label: "Observed", value: check.observed ? "Helmr administrator" : "Not a Helmr administrator", sub: check.observation_source ? `checked ${check.observation_source === "transaction" ? "when the change was saved" : "when the request arrived"}` : undefined },
        result(check),
      ];
    case "runtime_binding":
      return [
        ...(check.session_id ? [{ label: "Session", value: shortID(check.session_id), href: links.session(check.session_id), code: true }] : []),
        { label: "Generation", value: String(check.generation ?? "Not recorded"), sub: check.observed_generation ? `current was ${check.observed_generation}` : undefined },
        result(check),
        ...reason(check),
      ];
    case "principal_currency":
      return [
        ...person("For", check.subject_user_id, names, "member"),
        ...person("For", check.subject_api_key_id, names, "API key"),
        result(check),
        ...reason(check),
        ...(check.presented_version ? [{ label: "Versions", value: `Presented ${check.presented_version}, current ${check.observed_version}` }] : []),
      ];
    case "host_assignment":
      return [
        ...(check.host_id ? [{ label: "Worker host", value: shortID(check.host_id), code: true }] : []),
        ...(check.target_id ? [{ label: "Assigned to", value: `${sentence(check.target_kind ?? "target")} ${shortID(check.target_id)}`, sub: check.epoch ? `epoch ${check.epoch}` : undefined }] : []),
        result(check),
      ];
    default:
      return [
        ...Object.entries(check).filter(([key, value]) => key !== "type" && key !== "result" && value !== null && typeof value !== "object").map(([key, value]) => ({ label: sentence(key), value: String(value) })),
        result(check),
      ];
  }
}

const CREDENTIALS: Record<string, string> = {
  login_session: "Console sign-in session", api_key: "API key", worker_host: "Worker host credential", capacity_token: "Capacity token",
  provider_assertion: "Sign-in provider", magic_link: "Sign-in link", device_code: "Device sign-in",
};
const VERIFIED: Record<string, string> = { verified: "Verified on this request", failed: "Failed", absent: "No credential was presented" };

// How the actor signed in, as labelled values.
export function signInFields(event: AuditEvent): AuditField[] {
  const a = event.authentication;
  const restricted = "restricted" in event ? event.restricted : [];
  if (a.origin === "internal" && !restricted.includes("authentication")) return [
    { label: "Credential", value: "None; Helmr acted on its own", muted: true },
    ...(restricted.includes("transport") ? [{ label: "From", value: "Restricted", muted: true }] : []),
  ];
  return [
    ...(restricted.includes("authentication") ? [{ label: "Credential details", value: "Restricted", muted: true }] : []),
    ...(restricted.includes("transport") ? [{ label: "From", value: "Restricted", muted: true }] : []),
    ...(a.credential_kind ? [{ label: "Credential", value: CREDENTIALS[a.credential_kind] ?? sentence(a.credential_kind), sub: a.credential_version ? `version ${a.credential_version}` : undefined }] : []),
    ...(a.provider ? [{ label: "Provider", value: a.provider }] : []),
    { label: "Result", value: VERIFIED[a.result] ?? sentence(a.result), ...(a.result === "verified" ? { tone: "passed" as const } : a.result === "failed" ? { tone: "failed" as const } : {}) },
    ...("source_ip" in event && event.source_ip ? [{ label: "From", value: event.source_ip, code: true, sub: event.channel ? `over ${event.channel.toUpperCase() === "HTTP" ? "HTTP" : event.channel}` : undefined }] : []),
  ];
}
