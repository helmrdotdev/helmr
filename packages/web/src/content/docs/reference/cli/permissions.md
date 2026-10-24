---
title: Roles, groups and permissions
description: Manage scoped Role Grants and inspect member permissions.
sidebarLabel: role and group
---

# Roles, groups and permissions

These commands use `helmr login`, including when `HELMR_API_KEY` is set. Managing
Roles, Groups and Role Grants requires organization `access.manage`. A member
can inspect their own permissions. Environment API keys cannot manage this policy.

A Role defines reusable permissions. A Group contains people. A Role Grant assigns
one whole Role to a person, Group, or All members at an explicit scope.
Creating a Role or Group alone grants no access.

```sh
helmr role list
helmr role get --builtin agent_user
helmr role create "Operators" --permission sessions.manage --permission agents.use
helmr role copy --builtin session_viewer --name "Observers"
helmr role update "Observers" --description "Read production sessions"
helmr role delete "Observers" --yes

helmr group create Engineering
helmr group member add Engineering --user alice@example.com
helmr group member list Engineering
helmr group get Engineering
helmr group update Engineering --name "Platform engineering"
helmr group member remove "Platform engineering" --user alice@example.com --yes
helmr group delete "Platform engineering" --yes
```

Role and Group names match exactly, ignoring case. Scripts can use `--role-id`,
`--group-id` and `--member-id` instead. Member lookup by email requires management
access; members without an email must be selected by ID. `role get --builtin KEY`
selects a standard Role by its immutable key. Standard Roles cannot be edited or
deleted; copy one to create a custom definition.

Repeated `--permission` values replace the complete explicit permission set on
update. Omitted fields are preserved. Available IDs appear in help and shell
completion. Included permissions, such as View all sessions included by Manage all sessions,
are derived from the selected IDs. Updating a shared Role changes every grant of
that Role. An incompatible target or deleting an assigned Role is rejected.

Updates send the fetched revision in `If-Match`. A concurrent change fails rather
than overwriting silently. `--if-match REVISION` uses a revision from an earlier
read instead; inspect the new state before retrying a stale edit.

## Scoped Role Grants

```sh
helmr role grant create --group Engineering --role "Agent user" \
  --project operations --env production --agent invoice-checker
helmr role grant create --group Engineering --role Operators \
  --project operations --env development --all-agents
helmr role grant create --all-users --role "Agent user" \
  --project operations --env production --agent invoice-checker
helmr role grant create --user alice@example.com --role "Agent user" \
  --project operations --env production --agent invoice-checker
helmr role grant create --user alice@example.com --role "Access Administrator" --organization
helmr role grant list --group Engineering
helmr role grant list --project operations --env production --agent invoice-checker
helmr role grant get --id GRANT_ID
helmr role grant update --id GRANT_ID --agent invoice-checker --agent internal-faq --yes
helmr role grant delete --id GRANT_ID --yes
```

Workload creation requires an explicit Project and environment. If the Role
contains Agent actions, supply one or more `--agent` flags or explicitly choose
`--all-agents` to include future Agents. Other resource types automatically use
the entire selected environment. For example, a Role containing `agents.use`
and `secrets.manage` can use only invoice-checker while managing all secrets in
Production. Assign that same Role separately for Staging to choose another Agent.
Roles without explicit Agent actions accept no Agent-selection flags.

All members is the logical set of active organization members and accepts only
selected-Agent assignments containing `agents.use`. It is not a Group name or ID.
Organization assignments use `--organization` and no workload flags.

Identical creates return the existing assignment without another audit entry.
Different targets for the same recipient, Role and environment conflict. Use
`grant update --id ID` with the complete Agent selection and `--yes` to replace
targets. Updates and deletes send the fetched assignment revision in `If-Match`;
`--if-match REVISION` can pin a previously inspected version. Both creation and
update send the inspected Role revision. Conflicts require refreshed inspection,
not a blind retry. Recipient, Role and environment changes require a new assignment.

Human and JSON output expose each resource type's targets and implied actions.
Removing one assignment preserves rights supplied by other assignments. Ownership
alone supplies no ordinary permissions. List traverses all pages before printing.

## Member permissions

```sh
helmr member permissions --self
helmr member permissions --user alice@example.com \
  --project operations --env production --agent invoice-checker
```

The read-only result includes ownership separately from current capabilities and their direct, Group and All members sources. Membership IDs are stable only for the current membership; rejoining creates a new ID. Environment
and Agent scopes stay separate; an Agent grant does not imply Environment-wide
rights. Another member's result requires management access. Unknown or inaccessible
Agents are not revealed. This is a capability view, not a guarantee that a later
Session operation passes origin, credential or runtime admission checks.

Use `--json` for one success object on stdout. Command failures return nonzero;
permission command execution errors retain machine-readable codes in a JSON error
envelope. Destructive commands require `--yes`; without it they report affected
grants and perform no mutation.
