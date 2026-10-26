import { describe, expect, test } from "bun:test";
import type { AdminAuditEvent as AuditEvent } from "./audit";
import { actionLabel, actorOf, checkFields, eventSummary, scopeText, signInFields, targetLabel } from "./auditText";

const event = (overrides: Partial<AuditEvent>): AuditEvent => ({
  id: "e1", org_id: "o", project_id: null, environment_id: null, occurred_at: "2026-10-26T05:02:11Z", recorded_at: "2026-10-26T05:02:11Z",
  expires_at: "2027-01-24T05:02:11Z", retention_seconds: 7_776_000, actor_user_id: null, actor_api_key_id: null, actor_session_id: null,
  actor_worker_host_id: null, actor_component: null, actor_identity: null, runtime_generation: null, process_epoch: null,
  authentication: { result: "verified", origin: "request", provider: null, credential_kind: "login_session", credential_id: null, credential_version: 1 },
  deployment_id: null, session_id: null, turn_id: null, computer_id: null, cause_event_id: null, schema_version: 1,
  action: "role.created", outcome: "succeeded", target_kind: "role", target_id: "00000000-0000-7000-8000-000000000a01", target_user_id: null,
  slack_team_id: null, slack_user_id: null, target_resolution: "resolved", source_ip: null, channel: "http", operation_id: "op",
  authorization: { decision: "allowed", checks: [] }, causality: null, details: null, ...overrides,
});
const names = (entries: Record<string, string> = {}) => ({ get: (id: string) => entries[id] });

describe("actionLabel", () => {
  test("reads actions as short phrases", () => {
    expect(actionLabel("role_grant.created")).toBe("Created role assignment");
    expect(actionLabel("permission_group.member_added")).toBe("Added a member to group");
    expect(actionLabel("identity.sign_in")).toBe("Signed in");
    expect(actionLabel("session.content_read")).toBe("Read session content");
    expect(actionLabel("worker_pool.drained")).toBe("Worker pool drained");
  });
});

test("actorOf names people, keys, sessions and Helmr", () => {
  expect(actorOf(event({ actor_user_id: "u", actor_identity: { display_name: "Sato Kenji", email: "sato@acme.dev", source: "console" } }))).toEqual({ kind: "member", name: "Sato Kenji", detail: "sato@acme.dev", id: "u" });
  expect(actorOf(event({ actor_api_key_id: "00000000-0000-7000-8000-0000000000k1" })).kind).toBe("api_key");
  expect(actorOf(event({ actor_component: "retention_worker" }))).toEqual({ kind: "helmr", name: "Helmr", detail: "retention worker" });
  expect(actorOf(event({})).kind).toBe("unidentified");
});

test("targets use known names, then recorded details, then kind and short ID", () => {
  const e = event({});
  expect(targetLabel(e, names({ "00000000-0000-7000-8000-000000000a01": "Release manager" }))).toBe("Release manager");
  expect(targetLabel(event({ details: { name: "ci-deploy" }, target_kind: "api_key" }), names())).toBe("ci-deploy");
  expect(targetLabel(e, names())).toBe("role 000000000a01");
});

test("eventSummary says when Helmr refused", () => {
  expect(eventSummary(event({}), names({ "00000000-0000-7000-8000-000000000a01": "Release manager" }))).toBe("Created role · Release manager");
  expect(eventSummary(event({ action: "session.content_read", outcome: "denied", target_kind: "session" }), names())).toBe("Refused: Read session content · session 000000000a01");
});

const links = { session: (id: string) => `/sessions/${id}`, role: (id: string) => `/settings/roles/${id}` };

test("scopeText names the agent, environment or organization a permission applied to", () => {
  const env = "00000000-0000-7000-8000-000000000e01";
  expect(scopeText({ org_id: "o", environment_id: env, agent_id: "demo-triage" }, names({ [env]: "Production" }))).toBe("Agent demo-triage in Production");
  expect(scopeText({ org_id: "o", environment_id: env }, names({ [env]: "Production" }))).toBe("Production");
  expect(scopeText({ org_id: "o" }, names())).toBe("The whole organization");
});

test("a refused permission check says what was needed, where and why", () => {
  const fields = checkFields({ type: "permission", permission: "sessions.read", result: "failed", reason: "no_matching_grant", subject_user_id: "u1", scope: { org_id: "o", agent_id: "demo-triage" } }, names({ u1: "Sato Kenji" }), links);
  expect(fields.map((f) => [f.label, f.value])).toEqual([
    ["Needed", "sessions.read"],
    ["For", "Sato Kenji"],
    ["Where", "Agent demo-triage"],
    ["Result", "Failed"],
    ["Reason", "No role assignment gives this permission here"],
  ]);
});

test("a passed permission check shows the group, role and assignment that allowed it", () => {
  const fields = checkFields({ type: "permission", permission: "assignments.create", result: "passed", subject_user_id: "u1", scope: { org_id: "o" }, evidence: { subject: "group", group_id: "g1", role_id: "r1", role_revision: 4, assignment_id: "00000000-0000-7000-8000-0000000041aa", assignment_revision: 2 } }, names({ u1: "Aiko Mori", g1: "Access admins", r1: "Access Administrator" }), links);
  expect(fields.find((f) => f.label === "Through")?.value).toBe("Group Access admins");
  expect(fields.find((f) => f.label === "Role")).toMatchObject({ value: "Access Administrator", sub: "revision 4", href: "/settings/roles/r1" });
  expect(fields.find((f) => f.label === "Assignment")).toMatchObject({ value: "0000000041aa", sub: "revision 2" });
});

test("checks this Console does not name keep their recorded fields", () => {
  const fields = checkFields({ type: "future_check", result: "passed", widget: "w1" }, names(), links);
  expect(fields.map((f) => [f.label, f.value])).toEqual([["Widget", "w1"], ["Result", "Passed"]]);
});

test("signInFields reads the credential, result and origin", () => {
  expect(signInFields(event({ source_ip: "203.0.113.24" })).map((f) => [f.label, f.value])).toEqual([
    ["Credential", "Console sign-in session"],
    ["Result", "Verified on this request"],
    ["From", "203.0.113.24"],
  ]);
  expect(signInFields(event({ authentication: { result: "not_attempted", origin: "internal", provider: null, credential_kind: null, credential_id: null, credential_version: null } }))[0]!.value).toBe("None; Helmr acted on its own");
});


test("public internal events retain their no-credential explanation with restricted transport", () => {
  const { actor_worker_host_id, actor_component, runtime_generation, process_epoch, source_ip, cause_event_id, ...metadata } = event({});
  const projected: import("./audit").AuditEvent = {
    ...metadata, actor_kind: "platform_service", restricted: ["transport"],
    authentication: { result: "not_attempted", origin: "internal", provider: null, credential_kind: null, credential_id: null, credential_version: null },
    authorization: { decision: "not_evaluated", checks: [], installation_authority: false }, causality: {}, details: {},
  };
  expect(actorOf(projected)).toEqual({ kind: "platform_service", name: "Platform service" });
  expect(signInFields(projected)).toEqual([
    { label: "Credential", value: "None; Helmr acted on its own", muted: true },
    { label: "From", value: "Restricted", muted: true },
  ]);
});
