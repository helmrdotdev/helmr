import { statusBadgeClass } from "../../ui/styles";

export type SessionStatus = "open" | "completed" | "failed" | "closed" | "cancelled" | "expired";

const STATUS_LABELS: Record<SessionStatus, string> = {
  open: "Open",
  completed: "Completed",
  failed: "Failed",
  closed: "Closed",
  cancelled: "Cancelled",
  expired: "Expired",
};

export function sessionStatusTone(status: SessionStatus): "active" | "waiting" | "succeeded" | "revoked" | "expired" {
  if (status === "open") return "active";
  if (status === "completed" || status === "closed") return "succeeded";
  if (status === "expired") return "expired";
  return "revoked";
}

export function SessionStatusBadge(props: { status: SessionStatus }) {
  return (
    <span class={statusBadgeClass(sessionStatusTone(props.status))}>
      {STATUS_LABELS[props.status] ?? props.status}
    </span>
  );
}
