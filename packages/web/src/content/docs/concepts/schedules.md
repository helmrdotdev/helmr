---
title: Schedules
description: Source-declared cron triggers admit fresh Agent Sessions.
---

# Schedules

Attach `triggers.cron(id, expression, { timezone, input })` to an Agent definition.
The expression has five fields and the timezone is an IANA name. Deployment
promotion reconciles the Environment's active triggers.

Each fire admits a fresh independent Session and a fresh Computer using the
Agent's pinned definition. The Turn receives the declared text-part input array. Scheduled
work does not synthesize a human requester. Optional `slack: { channelId }` routing
uses an actual Slack channel ID and the Agent’s dedicated App. Promotion verifies
and pins that connection; every routed fire creates an explicit opening, including
quiet work. Occurrences during lost authorization are rejected without backfill.
Replacing the App connection requires re-promoting the schedule.

Occurrence identity includes the Environment, Agent, trigger and scheduled UTC
instant. Delivery retries reconcile that identity; they do not request another
business execution. Promotion defines an activation cutover. Times with no active Deployment are not
backfilled. Recovery considers only the latest due occurrence within the scheduler’s
bounded grace period.

Inspect Schedules through the client or CLI. The public surface is read-only:
change the source and promote a Deployment to change triggers. Use
`closeAfterIdle` or an explicit external close for the resulting Session's
lifetime. See [Schedule reference](/docs/reference/sdk/schedules).
