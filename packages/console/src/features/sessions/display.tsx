import { statusBadgeClass } from "../../ui/styles";

export type SessionStatus = "open" | "closed" | "cancelled" | "expired";
export type SessionActivity = "idle" | "queued" | "running" | "waiting";

const STATUS_LABELS: Record<SessionStatus, string> = {
  open: "Open",
  closed: "Closed",
  cancelled: "Cancelled",
  expired: "Expired",
};

const ACTIVITY_LABELS: Record<SessionActivity, string> = {
  idle: "Idle",
  queued: "Queued",
  running: "Running",
  waiting: "Waiting",
};

export function sessionStatusTone(status: SessionStatus): "active" | "waiting" | "succeeded" | "revoked" | "expired" {
  if (status === "open") return "waiting";
  if (status === "closed") return "succeeded";
  if (status === "expired") return "expired";
  return "revoked";
}

export function sessionActivityTone(activity: SessionActivity): "active" | "waiting" | "succeeded" | "revoked" | "expired" {
  if (activity === "running") return "active";
  if (activity === "queued" || activity === "waiting") return "waiting";
  return "succeeded";
}

export function SessionStatusBadge(props: { status: SessionStatus }) {
  return (
    <span class={statusBadgeClass(sessionStatusTone(props.status))}>
      {STATUS_LABELS[props.status] ?? props.status}
    </span>
  );
}

export function SessionActivityBadge(props: { activity: SessionActivity }) {
  return (
    <span class={statusBadgeClass(sessionActivityTone(props.activity))}>
      {ACTIVITY_LABELS[props.activity] ?? props.activity}
    </span>
  );
}
