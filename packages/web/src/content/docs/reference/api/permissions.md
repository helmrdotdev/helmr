---
title: Permission management API
description: Roles, permission groups, scoped Role Grants and member permissions.
sidebarLabel: Permissions
---

# Permission management API

The Console and management CLI use these `/api` routes with a human login session.
They do not accept Environment workload API keys. Every management operation
requires current `access.manage` in the session's organization. Ownership alone grants no ordinary access. The member
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
| GET | `/api/members/{member_id}/permissions` | Current effective capabilities and contributing sources |
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
Environment IDs and cannot contain workload actions. All members accepts only
`agents.use` with selected Agents, and starts with no assignments.

The catalog supplies `resource_type` and `included`
actions. Responses expose `targets` grouped by resource type, with mode, selected
Agent names and IDs, explicit permissions and included permissions. For example,
`agents.use`, `agents.manage`, `sessions.view` and `sessions.manage` all use the
same Agent selection in an assignment. Session management includes viewing for
those Agents; it does not grant Agent use. Executing new work in another person's
Session requires both management and use for that Agent, plus current runtime
authority. Use separate Roles and assignments when actions need different targets.

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
stored Agent targets. Physical Agent deletion removes only that target, advances
the assignment revision and records an audit event. An empty selected assignment
remains visible and grants no explicit Agent access; other resource types remain
effective. Repair it with a conditional PATCH or remove the assignment.

List filters are `member_id`, `group_id`, `api_key_id`, `role_id`, `subject_kind`, `scope_kind`,
`project`, `environment` and `agent`. Project/Environment filters use slugs; an
Agent filter requires both. Agent listing includes assignments selecting that Agent and all-Agent assignments. Pagination uses `limit`
(1–100, default 100), `cursor` and `next_cursor`. Role, Group and Group membership
lists use the same pagination contract. Membership PUT/DELETE is idempotent.

## Agent and Session actions

| Action | Access for the assigned Agents |
| --- | --- |
| `agents.use` | Start personal conversations and participate in configured shared Slack conversations |
| `agents.manage` | Configure Agent settings and connections |
| `sessions.view` | Read all Session history, including other people's personal conversations |
| `sessions.manage` | View, stop and close Sessions; new input, answers and resumption also require `agents.use` |

The standard Agent user Role has use; Agent settings manager has use and configure;
Session viewer has view; Session operator has use and manage. None embeds a target.
Computer access, deployments, secrets and administrative actions remain separate.
API keys receive ordinary Role Grants in their own environment, including selected
Agent targets. They do not inherit their creator's Roles, Group membership or
All members assignments. A Role assigned to a key may contain only `agents.use`,
`sessions.view`, `sessions.manage`, `deployments.publish`, `deployments.promote`,
`computers.operate` and `secrets.manage`. Incompatible assignments and subsequent
Role edits are rejected atomically. Role responses include `api_key_assignable`
so clients can offer eligible Roles without maintaining another permission list.

API-key issuance accepts `name`, optional `expires_in_days` and an optional `role`
object with `role_id`, `expected_role_revision`, `agents_mode` and `agent_ids`.
The Role assignment and key creation commit together. Without `role`, the key has
no workload access. Direct `permissions` or `scopes` are not accepted.

`keys.manage` controls creation, rotation, metadata updates and revocation;
`access.manage` controls Role assignments. Issuing a key with a Role requires both.
Listing key metadata is available to either an access administrator or a manager
of that environment's keys. Metadata includes current `role_names`. An environment
administrator without `access.manage` cannot assign Roles to a new key. Rotation
allows acting as an existing key and retains its Roles and Session ownership.
Key metadata updates never change Role assignments; use the Role Grants API.

## Effective permissions

Member permissions accepts optional `project`, `environment` and `agent` filters.
The result contains membership ID, member status, `is_owner` and `scopes`, each with public
`permissions` and `sources`. Sources are direct, Group or All members Role Grants. Ownership is reported separately.
Each grant source retains its original `target_kind`, optional `agent_id` and
`agent` key, configured `permissions`, and `included_permissions` with implied
capabilities. Removed members have no effective capabilities. An Agent scope includes applicable
Environment sources, but an Environment scope never absorbs Agent-only grants.
Self reads expose only the caller's contributing paths. Target visibility follows
the caller's authorization, not the inspected member's authority.

This read does not grant permissions directly to members or authorize a later
execution. Session origin, credential validity and lifecycle admission remain
operation-time checks. Permission definitions are fixed product capabilities;
they are not another user-managed collection.

## Ownership and initialization

A new organization has ten standard Roles, no custom Groups and no All members
Grants. Its creator is an Owner and receives two explicit organization Grants:
Access Administrator and Organization Manager. Creating an Environment grants
no workload access; assign an Environment Role separately.

An Owner can use `PUT /api/members/{member_id}/ownership` with `is_owner` and
`expected_is_owner` booleans. At least one active Owner must remain. Changing
ownership preserves ordinary Grants. `GET /api/ownership/members` provides the
minimal directory needed for succession. `POST /api/ownership/recover-access`
explicitly restores only the caller's fixed organization Access Administrator
Grant. These governance operations require current ownership and are audited.
They do not grant workload access automatically.
