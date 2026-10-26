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
membership changes requires Organization audit access. Ownership, platform administration, directory access and
execution-content permissions do not implicitly grant tenant audit access.
Environment API keys require an explicit grant and cannot read organization or
installation events. Audit metadata can disclose scoped account identity and
resource existence without granting access to execution content.

| Caller | Endpoint |
| --- | --- |
| Human login session | `GET /api/audit-events` |
| Environment API key | `GET /v1/audit-events` |
| Helmr administrator login | `GET /admin/api/v1/audit-events` |

A Helmr administrator (shown as **Helmr管理者** in the Console) manages one running Helmr service across its organizations.
This is a platform-wide user attribute, separate from organization ownership or
Roles; self-hosted operators can hold it too. Its grant is recorded as
`platform_admin.granted`, with the grant basis in details.

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
`target_kind`, `target_id`, `target_user_id`, `slack_team_id`, `slack_user_id`,
`operation_id`, `event_id`, and `outcome`. `action` can be repeated up to 100 times
to match any of the listed actions; each must be a recordable action. `operation_id`
selects every event one request or job recorded, and `event_id` selects one event;
both still apply only within `from` and `to`. Slack link identities use the
user/workspace/Slack-user triple rather than an invented target UUID. An installation query can specify `organization_id`;
omitting it selects installation facts. Tenant scope comes from authentication.

A login session can read the recordable actions, with each action's target kind,
outcomes and category, from `GET /api/audit-events/actions`. This list contains
organization actions only, including when the caller is a Helmr administrator.
Helmr administrators use `GET /admin/api/v1/audit-events/actions` for the full catalog.
Both lists are metadata and contain no audit data.

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
Former tenant credentials have no post-closure read route. A Helmr administrator can investigate by the original organization UUID. A replacement
organization with the same name cannot inherit that history. Access to live tenant
history is recorded in that tenant; requests for absent/deleted organizations are
recorded as unresolved installation facts. After all facts expire there is no
persistent audit inventory proving the former organization's existence.

## Identity and authority fields

The ordinary endpoints return a customer view, including for a caller who is also
a Helmr administrator. The separately authorized Admin endpoint returns
captured evidence. Both refer to the same event IDs; projection never rewrites the
stored actor or evidence. Cloud and self-hosted installations use this same access
boundary. Organization Owners and Auditors are not Helmr administrators.

Customer events have `actor_kind`: `user`, `api_key`, `session`,
`platform_service`, or `unidentified`. They retain at most one of `actor_user_id`,
`actor_api_key_id`, and `actor_session_id`. Host and internal-component actors appear
as `platform_service`, without a platform identifier. A Session remains the actor
when it performed an operation using a Worker credential. Account labels are
capture-time snapshots; human operators retain their user ID, name and email.

`authentication` records `origin` (`request` or `internal`), `result`, and the
credential/provider information permitted in that view. `authorization` separately
records `decision` (`allowed`, `denied`, or `not_evaluated`) and typed `checks`.
A verified credential can accompany a denied permission. Internal execution can
have no authorization check. The customer view sets
`authorization.installation_authority` when captured successful checks establish
that the human used installation authority. Current administrator status and a
failed administrator check do not establish that fact.

Permission checks preserve the subject, permission, visible scope, and granting
assignment and Role revisions, or the failure reason. Customer responses withhold
machine credentials, Host/Pool/Group identifiers, runtime generations/process
epochs, internal authorization checks and unqualified peer IP addresses. They also
withhold causality links without evidence that the reader can see their scope.
`restricted` names the sections containing withheld evidence; it does not mean
that authentication or authorization was not evaluated. Per-event retention and
expiry remain visible. The Admin view retains raw captured evidence, including
`actor_worker_host_id`, `actor_component`, source IP and runtime checks.

Customer `details` and authorization checks use closed, action-specific types.
An eligible record that cannot be safely projected makes the whole page unavailable;
no partial page or raw JSON fallback is returned. Retained facts with unsupported
action names or prior payload shapes remain unchanged and available to Admin
readers until expiry. Platform-only operations, such
as Worker supply management and audit maintenance, are excluded before pagination.
Customer search rejects raw `worker_host`/`component` actor filters and platform
resource/action filters. `actor_id` selects only visible user, API-key or Session
identities, even without `actor_kind`; it cannot accompany `platform_service` or
`unidentified`. Admin actor filters retain the raw vocabulary.

Admin access to live tenant history remains a tenant-visible human access fact.
Its customer details keep the selected time range and visible scope, while
withholding internal filters and result counts. Search receipts retain which read
model performed the selection; historical receipts without that provenance also
withhold filters/counts that could reveal hidden platform activity.

`causality` identifies an initiator or source independently. A Session's creator
is not the actor of its later runtime operations. Permission mutations describe
the changed resource in target/details; their authorization records which
existing authority allowed the mutation.

## Recorded operations and attention reads

Project and Environment creation/update and newly published Deployments record
facts in the same transaction as the mutation. Default Environments created with
a Project are included. Reusing an existing Deployment does not create another
publication fact. Session creation has one `session.created` fact; each admitted
Turn has `turn.admitted`. Successful login records `identity.sign_in` once in each
applicable installation/organization custody scope.

`session.close_requested`, `session.cancel_requested`,
`session.interrupt_requested`, `session.resume_requested`, and
`turn.interrupt_requested` record accepted control requests. Terminal events
record their actual effects separately. `template.refresh_requested` likewise
records acceptance, including a new request that joins an existing build.
`audit.retention_observed` records a process's observed retention policy.
Checkpoint, hold and image-build references use `checkpoint_id`, `hold_id` and
`image_build_id`; `request_id` is reserved for actual requests.

`GET /api/projects/{projectID}/environments/{envID}/attention/counts` returns
complete authorized counts without execution content. Question/hold/failed-Turn
counts are omitted when Session reads are unauthorized; Slack reauthorization
counts require their own permission. No authorized category returns forbidden.
This metadata read does not create audit facts. The Console badge polls counts
every ten seconds; Overview requests the full attention response while displayed.
A full attention response records content disclosure for the distinct union of
Sessions represented by questions, holds and failed Turns. A capture failure
withholds that content. Counts and content are separate snapshots.

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
