---
title: Permission management API
description: Roles, permission groups, scoped Role Grants and member permissions.
sidebarLabel: Permissions
---

# Permission management API

The Console and management CLI use these `/api` routes with a human login session.
They do not accept Environment workload API keys. Each operation requires its own organization permission: `roles.read/create/update/delete`,
`groups.read/create/update/delete/add_member/remove_member`, or
`assignments.read/create/update/delete`. Reading another member's permissions
requires `assignments.read`; resolving assignment targets requires `assignments.read`. Ownership alone grants no ordinary access. The member
permissions read also permits an ordinary active member to inspect themselves.

| Method | Path | Result |
| --- | --- | --- |
| GET, POST | `/api/roles` | List definitions with their available permission catalog; create a custom Role |
| GET, PATCH, DELETE | `/api/roles/{id}` | Read, partially edit or delete a Role |
| GET, POST | `/api/groups` | List or create Groups |
| GET, PATCH, DELETE | `/api/groups/{id}` | Read, rename or delete a Group |
| GET | `/api/groups/{id}/members` | Current member IDs |
| PUT, DELETE | `/api/groups/{id}/members/{member_id}` | Ensure or remove a membership |
| GET, POST | `/api/role-grants` | List or ensure scoped grants |
| GET, PATCH, DELETE | `/api/role-grants/{id}` | Read, replace targets or remove one assignment |
| GET | `/api/members/{member_id}/permissions` | Current effective permissions and contributing sources |
| GET | `/api/projects/{project}/access-targets` | Resolve Environment slugs and Agent keys to IDs for management |

Role create accepts `name`, `description` and a nonempty `permissions` array.
Role PATCH changes only supplied fields; `permissions` replaces the complete set.
Group create/PATCH accepts `name`. Definitions return an integer `revision`; GET
and writes also return its quoted ETag. PATCH requires `If-Match: "REVISION"`.
Missing preconditions return 428; a stale revision returns 412 with
`stale_revision`. A standard Role cannot be changed or deleted. An assigned Role
cannot be deleted, and a shared edit must remain compatible with every grant.
The errors `role_in_use` and `role_target_incompatible` include the conflicting
`grant_ids` in their `details`.

## Role assignments and targets

```json
{
  "subject": {"type": "member", "member_id": "MEMBERSHIP_UUID"},
  "role_id": "ROLE_UUID",
  "expected_role_revision": 3,
  "project_id": "PROJECT_UUID",
  "environment_id": "ENVIRONMENT_UUID",
  "agents_mode": "selected",
  "agent_ids": ["AGENT_UUID"]
}
```

A subject is a `member` with `member_id`, a `group` with `group_id`, an `api_key` with `api_key_id`, or
`all_members` without another ID. Membership IDs differ from user IDs; `/api/me`
and Members responses expose both. Rejoining creates a new membership ID.

A workload assignment covers one environment. Explicit Agent actions use
`agents_mode`: `selected` requires 1–100 Agent IDs in that environment; `all`
includes current and future Agents. `none` is required when the Role has no
explicit Agent actions. Only `selected` accepts IDs. Other resource types apply
to the entire environment. Organization Roles use `none` without Project or
Environment IDs. A Role must have at least one assignment level supported by
every included permission. All members accepts only
`agents.use` with selected Agents, and starts with no assignments.

The catalog supplies `resource_type`, `scope_types`, `assignment_levels` and
`api_key_assignable` for each action. `scope_types` describes the resource scopes
on which the permission operates; `assignment_levels` lists where the permission
can be assigned; a Role returns the intersection for all its permissions. Permissions are exact: no action implies another. Responses expose
`targets` grouped by resource type, including the effective mode, selected Agent
names and IDs, and permissions. Agent, connection, Session and Trigger actions
share the assignment's Agent selection. Environment actions apply throughout the
chosen environment. A Role can combine these two kinds of actions. Organization
actions cannot be mixed with either unless every permission supports that
assignment level. `audit_events.read` supports Organization and Environment
assignments; an audit-only Role can use either. Mixing it with Organization-only
actions requires Organization assignment; mixing it with Environment/Agent actions
requires Environment assignment and preserves the Agent selection. See
[audit events](/docs/reference/api/audit-events) for disclosure and reader boundaries.

Sending input, answering questions or resuming somebody else's Session requires
the corresponding `sessions.send`, `sessions.answer` or `sessions.resume` action
and `agents.use` for that Agent. Existing personal Session origin rules still
apply. Use separate Roles and assignments when actions need different targets.

POST returns `{"grants":[...]}` with one header and `created: true|false`.
There is one assignment per recipient, Role and environment. Equivalent normalized
POSTs return the existing assignment (200); new assignments return 201. Different
targets for that identity return 409 `assignment_exists` with its ID and revision.
POST never unions or overwrites existing targets.

PATCH uses the same complete input and `If-Match: "ASSIGNMENT_REVISION"`.
It replaces all targets atomically and preserves recipient, Role and environment.
Both POST and PATCH require the inspected `expected_role_revision`; a changed
Role returns 412 and requires a refreshed access summary. DELETE requires the
observed assignment revision in `If-Match`, returning 412 for stale state and
204 for an already absent assignment. Missing required preconditions return 428.

Role edits preserve Agent targets when adding environment-only actions. Adding
Agent actions to assignments without configured targets is rejected atomically
with conflicting `grant_ids`. Removing the last explicit Agent action clears
stored Agent targets. Each assignment mutation advances its revision once and
records an audit event in the same transaction.

[Agent deletion](/docs/reference/api/agents) requires a human session with
`agents.delete` on the target. Restore requires `agents.restore`. Neither requires
assignment or connection management permissions. Deletion includes fixed cleanup
of the Agent's connection and selected access edges. It removes that Agent from selected
assignments. Assignments with no remaining effect are removed; assignments that
still grant other Agent targets or non-Agent permissions are preserved. An empty
selected assignment grants no explicit Agent access, while its other resource
permissions remain effective. Conditional PATCH and assignment removal remain
available. Deletion advances revisions of preserved affected assignments, so a
stale edit must refresh before retrying. Restore does not recreate removed
selections or assignments; broad assignments apply to the restored Agent again.

List filters are `member_id`, `group_id`, `api_key_id`, `role_id`, `subject_kind`, `scope_kind`,
`project`, `environment` and `agent`. Project/Environment filters use slugs; an
Agent filter requires both. Agent listing includes assignments selecting that Agent and all-Agent assignments. Pagination uses `limit`
(1–100, default 100), `cursor` and `next_cursor`. Role, Group and Group membership
lists use the same pagination contract. Membership PUT/DELETE is idempotent.

## Action catalog

Each permission below authorizes one operation. Organization actions apply to the
organization; Environment actions to one environment; Agent actions to selected
Agents or all current and future Agents in that environment.

| Permission | Scope | API keys | Meaning |
| --- | --- | --- | --- |
| `agents.read` | Agent | Yes | List/read the authorized Agent's definition and retained status, including deleted status; not other Agents or full deployment catalogs. |
| `agents.use` | Agent | Yes | Start ordinary own-origin conversations and perform the established own-session operations; participate in authorized connected shared conversations. Includes only identity/definition needed to use the target Agent. |
| `agents.delete` | Agent | No | Preview and apply explicit Agent deletion, including its fixed cleanup effects; current-deployment and unsettled-work blockers remain. |
| `agents.restore` | Agent | No | Preview and restore a deleted Agent identity. Does not restore removed selected grants or Slack connections; broad grants apply again and existing parent Sessions may create children immediately, before another deployment includes the Agent. |
| `agent_connections.read` | Agent | No | Read the Agent's Slack connection status/setup metadata, without credential values. |
| `agent_connections.create` | Agent | No | Begin connection/setup registration for the Agent. |
| `agent_connections.update_credentials` | Agent | No | Replace supplied Slack app credentials; no stored-value export. |
| `agent_connections.authorize` | Agent | No | Begin/complete installation authorization or reauthorization for this Agent's connection, subject to verified OAuth flow. |
| `agent_connections.delete` | Agent | No | Disconnect the Agent's connection and perform its fixed cleanup. |
| `sessions.read` | Agent | Yes | Read/list matching Sessions, Turns, events, Asks, updates and execution history. Includes conversation contents and the existing human delivery-record view. |
| `sessions.create_keyed` | Agent | Yes | Supply a caller-defined Session key or the existing administrative channel identifier on scoped Session creation; also requires agents.use. Does not grant somebody else's origin. |
| `sessions.send` | Agent | Yes | Enqueue input or send a message to a matching Session/Turn; also requires agents.use. |
| `sessions.answer` | Agent | Yes | Answer an Ask; also requires agents.use. |
| `sessions.resume` | Agent | Yes | Resume matching suspended work; also requires agents.use. |
| `sessions.interrupt` | Agent | Yes | Interrupt the addressed Session/Turn using existing interruption semantics. |
| `sessions.cancel` | Agent | Yes | Cancel the addressed Session using existing cancellation semantics. |
| `sessions.close` | Agent | Yes | Inspect the close-scope preview and request Session closure using existing child/settlement constraints. |
| `sessions.check_delivery` | Agent | No | Request Slack publication reconciliation for an uncertain delivery; also requires Session read. No resend or new execution authority. |
| `sessions.abandon_delivery` | Agent | No | Record the existing delivery-abandonment disposition; also requires Session read. No resend or new execution authority. |
| `deployments.read` | Environment | Yes | Read deployment list, current deployment, record and events; not Session conversations or Computer stdout/stderr. |
| `deployments.publish` | Environment | Yes | Obtain upload plan, finalize bundle and register immutable deployment definitions/code. Publication/preparation may execute code with configured credentials. |
| `deployments.promote` | Environment | Yes | Activate or roll back to an existing deployment; changes current definitions and scheduled admission. No upload authority. |
| `templates.read` | Environment | Yes | Read Template definitions and preparation-build status for permitted deployment selection. No broad Agent/Session authority. |
| `templates.refresh` | Environment | Yes | Start explicit preparation refresh for a current Template, including configured preparation credential use. |
| `triggers.read` | Agent | Yes | Read matching trigger definitions, input payloads and occurrence records. This is potentially sensitive input, not a metadata-only feed. Session links still require Session authority to follow. |
| `computers.read` | Environment | Yes | List/read Computer state, resource diagnostics, execution-scope preview, usage IDs and Command status/outcome. No conversation/file/stdout/stderr content. |
| `computers.read_output` | Environment | Yes | Read retained Command stdout/stderr. This can disclose sensitive content even without execution. |
| `computers.execute` | Environment | Yes | Submit Commands on an existing Computer or explicitly place an authorized Session there. Does not implicitly grant historical output/status APIs; standard debugging Roles include them. |
| `computers.cancel_command` | Environment | Yes | Cancel a Command in the environment, regardless of creator. No new owner-only cancellation mechanism. |
| `computers.create` | Environment | Yes | Provision from an allowed definition, including requested Secret bindings and initial retention, under existing quota and creation rules. This is trusted credential-bearing provisioning. |
| `computers.wake` | Environment | Yes | Explicitly wake a Computer, subject to existing state constraints. |
| `computers.hibernate` | Environment | Yes | Explicitly hibernate a Computer with existing busy/settlement checks. |
| `computers.update_retention` | Environment | Yes | Change deletion-after-unused policy; shortening it can promptly cause deletion. |
| `computers.delete` | Environment | Yes | Delete the Computer through the platform API with existing settlement rules. |
| `secrets.read` | Environment | Yes | Read Secret names/IDs, versions and non-value metadata. Never retrieve stored plaintext. |
| `secrets.create` | Environment | Yes | Create a Secret using newly supplied material. |
| `secrets.rotate` | Environment | Yes | Replace/rotate supplied Secret material. |
| `secrets.revoke` | Environment | Yes | Revoke a Secret under existing binding/lifecycle constraints. |
| `api_keys.read` | Environment | No | List key metadata and current non-secret status. Access-assignment UI may see minimal key identity through its target-directory rule. |
| `api_keys.create` | Environment | No | Issue a key and reveal its credential once. The new key has no workload Role until separately assigned. |
| `api_keys.update` | Environment | No | Update a key's name and expiration, including extending its usable lifetime; never its Role grants. |
| `api_keys.rotate` | Environment | No | Rotate and receive a replacement credential. Can act with that key's assigned authority. |
| `api_keys.revoke` | Environment | No | Revoke a key. |
| `projects.read` | Organization | No | Read the organization-wide Project directory, full Project metadata and history-retention settings. |
| `projects.create` | Organization | No | Create a Project in the organization. |
| `projects.update` | Organization | No | Change a Project's display name; no slug/identity reassignment. |
| `projects.update_retention` | Organization | No | Change Project history retention; explicitly potentially destructive to retained history. |
| `environments.read` | Organization | No | Read full Environment metadata in the organization. |
| `environments.create` | Organization | No | Create an Environment under an existing Project. |
| `environments.update` | Organization | No | Update an Environment's display name and color; no identity/region move. |
| `members.read` | Organization | No | List current members and identity metadata. |
| `members.remove` | Organization | No | Remove membership and its fixed cleanup; last-owner protections remain. |
| `invitations.read` | Organization | No | List invitations. |
| `invitations.create` | Organization | No | Invite members; new members receive existing All-members access where configured. |
| `invitations.revoke` | Organization | No | Revoke an invitation. |
| `groups.read` | Organization | No | List/read Groups and their members. |
| `groups.create` | Organization | No | Create an empty Group. |
| `groups.update` | Organization | No | Rename/update Group metadata, not its members. |
| `groups.delete` | Organization | No | Delete a Group, its memberships and its assignments, immediately revoking the access they conferred. Unlike Role deletion, this has no in-use blocker. |
| `groups.add_member` | Organization | No | Add a member to a Group, immediately conveying its assigned Roles. |
| `groups.remove_member` | Organization | No | Remove a member from a Group. |
| `roles.read` | Organization | No | Read Role definitions and the Permission catalog. |
| `roles.create` | Organization | No | Create a custom Role with an explicit Permission set; does not assign it. |
| `roles.update` | Organization | No | Modify a custom Role; changes the authority of all existing assignments. Standard Roles remain immutable to users. |
| `roles.delete` | Organization | No | Delete a custom Role subject to in-use constraints. |
| `assignments.read` | Organization | No | Read assignments, other members' effective-access explanation and the minimal assignment-target directory. |
| `assignments.create` | Organization | No | Assign a Role to an eligible subject and valid scope. This can delegate authority the caller does not personally hold, as current Access administration can. |
| `assignments.update` | Organization | No | Change an assignment's Role/targets/eligible properties; same delegation power. |
| `assignments.delete` | Organization | No | Remove an assignment. |

API keys receive Role Grants in their own environment. They do not inherit their
creator's Roles, Group membership or All members assignments. Every permission
in a key's Role must be API-key assignable. Incompatible assignments and later
Role edits are rejected atomically. Role responses include `api_key_assignable`.

API-key issuance accepts `name`, optional `expires_in_days` and an optional `role`
object with `role_id`, `expected_role_revision`, `agents_mode` and `agent_ids`.
The Role assignment and key creation commit together. Without `role`, the key has
no workload access. Direct `permissions` or `scopes` are not accepted.

`api_keys.read`, `api_keys.create`, `api_keys.update`, `api_keys.rotate` and
`api_keys.revoke` independently control key metadata and lifecycle. Issuing a key
with a Role also requires `assignments.create`. Without it, create the key without
a Role; an assignment administrator can grant access later. Metadata includes
current `role_names`. Rotation allows acting as the existing key and retains its
Roles and Session ownership. Metadata updates never change assignments.

## Effective permissions

Member permissions accepts optional `project`, `environment` and `agent` filters.
The result contains membership ID, member status, `is_owner` and `scopes`, each with public
`permissions` and `sources`. Sources are direct, Group or All members Role Grants. Ownership is reported separately.
Each grant source retains its original `target_kind`, optional `agent_id` and
`agent` key and exact `permissions`. Removed members have no effective permissions. An Agent scope includes applicable
Environment sources, but an Environment scope never absorbs Agent-only grants.
Self reads expose only the caller's contributing paths. Target visibility follows
the caller's authorization, not the inspected member's authority.

This read does not grant permissions directly to members or authorize a later
execution. Session origin, credential validity and lifecycle admission remain
operation-time checks. Permission definitions are fixed product capabilities;
they are not another user-managed collection.

## Ownership and initialization

A new organization has sixteen standard Roles, no custom Groups and no All members
Grants. Its creator is an Owner and receives two explicit organization Grants:
Access Administrator and Organization Structure Administrator. Creating an Environment grants
no workload access; assign an Environment Role separately.

An Owner can use `PUT /api/members/{member_id}/ownership` with `is_owner` and
`expected_is_owner` booleans. At least one active Owner must remain. Changing
ownership preserves ordinary Grants. `GET /api/ownership/members` provides the
minimal directory needed for succession. `POST /api/ownership/recover-access`
explicitly restores only the caller's fixed organization Access Administrator
Grant. These governance operations require current ownership and are audited.
They do not grant workload access automatically.
