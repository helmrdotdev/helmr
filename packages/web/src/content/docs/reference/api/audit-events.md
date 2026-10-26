---
title: Audit events
description: Investigate retained security activity with scoped access and explicit retention.
sidebarLabel: Audit events
---

# Audit events

Audit events record Helmr-enforced administrative operations, authentication,
Session/Turn and Command transitions, mediated secret exposure, covered refusals,
and access to execution content or audit records. They contain security metadata,
account identity and evaluated authority. They do not contain prompts, messages,
command arguments/output, file contents, tokens or secret values. Admission does
not prove completion; secret exposure does not prove use. Native activity outside
Helmr's enforcing boundaries is not observed by this log.

## Access and search

The exact permission `audit_events.read` can be assigned at Organization or
Environment scope using the standard **Auditor** Role or a custom Role. An
Organization assignment includes organization events and its environments; an
Environment assignment includes that environment's events. Several Environment
grants form a union. Environment history excludes Organization IAM changes, so it
cannot reconstruct every change to effective access. Investigating Role, group and
membership changes requires Organization audit access. Ownership, installation administration, directory access and
execution-content permissions do not implicitly grant tenant audit access.
Environment API keys require an explicit grant and cannot read organization or
installation events. Audit metadata can disclose scoped account identity, source
IP and resource existence without granting access to their execution content.

| Caller | Endpoint |
| --- | --- |
| Human login session | `GET /api/audit-events` |
| Environment API key | `GET /v1/audit-events` |
| Installation administrator login | `GET /admin/api/v1/audit-events` |

The Console provides **Settings → Audit events** and a separate installation audit
view for administrators. The CLI reads one page:

```sh
helmr audit list --from 2026-10-01T00:00:00Z --to 2026-10-02T00:00:00Z --json
helmr installation audit list --from 2026-10-01T00:00:00Z --to 2026-10-02T00:00:00Z --organization-id ORIGINAL_ORGANIZATION_UUID --json
```

The installation command requires a login session. The ordinary command honors the
selected session or explicitly configured Environment key; it never silently uses
an administrator route to bypass a denied tenant read.

Required `from` and `to` parameters are RFC3339 timestamps: inclusive start and
exclusive end, at most 31 days apart. `limit` defaults to 100 and accepts 1–500.
Optional exact filters are `environment_id`, `actor_kind`, `actor_id`, `action`,
`target_kind`, `target_id`, `target_user_id`, `slack_team_id`, `slack_user_id`, and
`outcome`. Slack link identities use the user/workspace/Slack-user triple rather
than an invented target UUID. An installation query can specify `organization_id`;
omitting it selects installation facts. Tenant scope comes from authentication.

The response contains `events` and, when another bounded page exists, `next_before`
with `recorded_at` and `id`. Send these as `before_recorded_at` and `before_id`,
together with the same range and filters, to request older events. Each request
reauthorizes, excludes expired rows, and records access before returning data,
including an empty result. If a union of Environment grants exceeds the bounded
authorization evidence size, the API returns an invalid-input response asking for
`environment_id`; a narrowed search uses only that Environment’s authority evidence.
Capture failure returns no selected records.

Pagination is for investigation. Timestamps and UUIDs do not establish commit
order; late commits and expiry can change pages. Repeat a time range when
investigating. This endpoint is not a loss-free collector, archive or export
checkpoint. Collection and independent custody require separate capabilities.

After organization deletion, its original IDs and unexpired audit facts remain.
Former tenant credentials have no post-closure read route. An installation
administrator can investigate by the original organization UUID. A replacement
organization with the same name cannot inherit that history. Access to live tenant
history is recorded in that tenant; requests for absent/deleted organizations are
recorded as unresolved installation facts. After all facts expire there is no
persistent audit inventory proving the former organization's existence.

## Identity and authority fields

An event references at most one actor: `actor_user_id`, `actor_api_key_id`,
`actor_session_id`, `actor_worker_host_id`, or `actor_component`. No actor is
established for failed or absent authentication. `actor_kind` is a search filter
with values `user`, `api_key`, `session`, `worker_host`, `component`, or `unidentified`;
it is derived from these references. Account labels are capture-time snapshots.

`authentication` records `origin` (`request` or `internal`), `result`, and actual
credential/provider information. `authorization` separately records `decision`
(`allowed`, `denied`, or `not_evaluated`) and typed `checks`. A verified credential
can accompany a denied permission. Internal execution can have no authorization
check. Installation administration records the same user ID with the observed
`admin` attribute; it does not create a separate administrator actor or Role.

Permission checks identify the checked subject, permission and scope. A passed
check includes its granting assignment and Role revisions; a failed check gives
the observed reason without inventing a grant. Runtime checks identify the
Session binding or Host assignment. Credential refresh failures retain the
admission authentication and describe the later mismatch. These are observations
of the owner's checks, not an executable policy document.

`causality` identifies an initiator or source independently. A Session's creator
is not the actor of its later runtime operations. Permission mutations describe
the changed resource in target/details; their authorization records which
existing authority allowed the mutation.

## Retention and deployment prerequisites

Every producing control-plane/dispatcher process must receive a finite positive
`AUDIT_RETENTION_SECONDS`. There is no implicit duration, unlimited mode or tenant
override. Startup records the process's observed value before service begins;
readiness validates policy and effective database privileges. A rolling deployment
can temporarily apply different durations, retained on each fact.

`recorded_at` is database statement time, not commit time. `expires_at` is fixed
from that time and the row's stored duration. Old source occurrence times do not
shorten new facts; long open transactions do not gain time after commit. Policy
changes affect new facts only. They cannot extend, shorten or restore old history.
Expired facts are hidden from all product readers even while cleanup is late.
Physical page reclamation, backup retention, legal hold and selective identity
erasure are separate concerns; immediate secure erasure is not promised.

Use separate connections:

| Setting | Process / authority |
| --- | --- |
| `DATABASE_MIGRATION_URL` | Schema migration owner; never mounted in HTTP runtime |
| `DATABASE_URL` | Control-plane/dispatcher runtime; append/read audit facts |
| `DATABASE_AUDIT_MAINTENANCE_URL` | Dedicated purge/restore command; never mounted in HTTP runtime |

Before a deployment serves users, its operator must select retention from storage
and volume requirements, configure the separate roles, and schedule the following
command through the deployment's native scheduler:

```sh
control-plane audit purge
```

The job requires `AUDIT_RETENTION_SECONDS`, `AUDIT_MAINTENANCE_TIMEOUT_SECONDS`
(1–3600), and `AUDIT_PURGE_MAX_BATCHES` (1–1000). A batch deletes at most 1,000
eligible rows and commits its count/expiry summary atomically. An idle invocation
creates no success event. Concurrent invocations report only rows they delete.
Helmr's AWS modules install a dedicated Fargate purge task and EventBridge schedule
alongside the serving service. The task receives only the maintenance credential.
`audit_purge_interval_minutes`, `audit_maintenance_timeout_seconds`, and
`audit_purge_max_batches` configure the interval and finite work; defaults are
15 minutes, 300 seconds and 50 batches. The timeout must cover every five-second
batch deadline plus 15 seconds of setup overhead and remain shorter than the schedule interval. A CloudWatch alarm treats absence of a
successful invocation across two intervals as a failure, including a task that
could not start. Set `audit_maintenance_alarm_actions` to existing alert destinations.
See [AWS task failure observations](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/task-lifecycle-events.html)
when investigating launch and runtime failures. These definitions still require
operator-reviewed application and qualification in the target installation.

The scheduler interval and resource limits are explicit deployment choices; the
application does not run its own purge daemon. Monitor job exit status, last
successful invocation, expired-row backlog and database storage pressure outside
the expiring audit table. Size aggregate load across all application replicas:
request admission limits are per process, not distributed quotas.

Covered ordinary HTTP requests have bounded per-process rate/concurrency and
per-principal concurrency limits, with rate limits for capture-bearing routes. The current process ceilings
are 400 requests/second (burst 800), 16 concurrent ordinary operations, and
100 capture-bearing requests/second per principal (burst 200), with at most twelve
in-flight operations per principal, including metadata/navigation reads. Those
reads do not spend the principal capture rate budget. The bounded principal cache evicts
its oldest entry at capacity; this is an abuse backstop, not strict distributed or
tenant-isolated fairness. Containment uses available ordinary capacity before a
separate 10/second (burst 20), four-operation reserve. Worker refusals have their
own 20/second (burst 40), eight-operation budget. Overload
returns 429 before work is admitted; established SSE streams release their operation
slot. Mandatory capture failure fails the operation and reports bounded operational
health/log counters. Limits do not sample away facts for admitted operations.

For database recovery, keep product admission fenced using the deployment's
recovery procedure. Preserve restored IDs, recorded times and expiry. Validate
privileges/policy, run bounded expiry cleanup, and record the recovery gap before
reopening:

```sh
control-plane audit record-restore \
  --backup-reference NONSECRET_BACKUP_REFERENCE \
  --unknown-from 2026-10-01T01:00:00Z \
  --unknown-to 2026-10-01T02:00:00Z
```

Keep recovery evidence outside the restored database as well. This operation does
not recover facts absent from the backup, renew their expiry, or assert a complete
history. Maintenance credentials are trusted: their DELETE privilege can technically
remove unexpired records. Runtime privilege separation does not protect against
database owners or a compromised trusted producer.
