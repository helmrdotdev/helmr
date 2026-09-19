# ClickHouse deployment access

The Control Plane reads telemetry, the dispatcher inserts it, and the one-off
migration task creates the telemetry database and tables. Give each a separate
username and Secrets Manager password. Application identities must not use
`default`. Each ECS execution role can retrieve only its task's ClickHouse
password. Migration has its own execution role.

The generic Control Plane module has explicit `clickhouse_access_mode`:

- `external`: the three application users and grants already exist. No
  administrator secret or account bootstrap task is configured.
- `bootstrap`: a fourth administrator credential provisions the three users
  through `control-plane clickhouse-bootstrap`. Only its one-off ECS task receives
  the administrator password and the three application passwords. It has no task
  IAM role and uses the existing Control Plane image and network.

The administrator needs access-management privileges to create users, inspect
users/grants/settings and grant the listed privileges. It remains an operational
credential, never a runtime application credential. Bootstrap receives
`CLICKHOUSE_URL`, `CLICKHOUSE_BOOTSTRAP_USER/PASSWORD`, and
`CLICKHOUSE_READER_USER/PASSWORD`, `CLICKHOUSE_INGESTER_USER/PASSWORD`,
`CLICKHOUSE_MIGRATION_USER/PASSWORD`. Secrets are injected by ECS, not read from
AWS by the command. HTTPS is required except for disposable loopback testing.

Direct application grants are:

| User | Privileges on `helmr_telemetry.*` |
|---|---|
| Reader | SELECT |
| Ingester | INSERT |
| Migration | CREATE DATABASE, CREATE TABLE |

Future migration operations must add their specific required privilege deliberately.
No application user receives access-management privileges or grant options. The
reader's settings lock read-only access and throwing scan, timeout and result
overflow behavior, including when a server profile supplies a result-row cap. Its
positive scan/memory ceilings match the historical query limits. The 35-second
server maximum accommodates the Go driver's five-second augmentation of the
30-second client deadline; the client still cancels at its own deadline.

Bootstrap validates every existing user's supplied credential, direct grants,
role memberships and owned settings before changing any user or privileges.
Applicable SQL settings profiles are rejected, including `TO ALL` and
`TO ALL EXCEPT` assignments; exclusions that omit all application users are safe.
Unexpected access or a credential mismatch fails. It creates missing users with
SHA-256 password verifiers and adds missing expected grants. It never logs SQL,
passwords or password hashes, changes a password, drops users/databases, or
revokes unexpected grants. Repeat a failed partial bootstrap with the same
credentials after correcting its cause.

Passwords are create-once deployment credentials. Changing a secret alone is not
rotation: running tasks retain their injected value and the database retains the
old password. Coordinate an explicitly authorized database credential change,
secret update and task replacement; bootstrap refuses to silently reconcile a
mismatch. Inspect and resolve privilege drift explicitly before retrying.

AWS source validation uses provider mocks and a disposable local ClickHouse
server. It does not establish deployed Cloud grants, network reachability,
profile compatibility or workload capacity. Validate the exact deployed version
and accounts before accepting a production deployment.
