import { statusBadgeClass } from "../../ui/styles";

export type TaskSessionStatus = "open" | "completed" | "failed" | "closed" | "cancelled" | "expired";

const STATUS_LABELS: Record<TaskSessionStatus, string> = {
  open: "Open",
  completed: "Completed",
  failed: "Failed",
  closed: "Closed",
  cancelled: "Cancelled",
  expired: "Expired",
};

export function sessionStatusTone(status: TaskSessionStatus): "active" | "waiting" | "succeeded" | "revoked" | "expired" {
  if (status === "open") return "active";
  if (status === "completed" || status === "closed") return "succeeded";
  if (status === "expired") return "expired";
  return "revoked";
}

export function TaskSessionStatusBadge(props: { status: TaskSessionStatus }) {
  return (
    <span class={statusBadgeClass(sessionStatusTone(props.status))}>
      {STATUS_LABELS[props.status] ?? props.status}
    </span>
  );
}
