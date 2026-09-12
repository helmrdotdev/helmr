import type { SessionStatus } from "../../lib/sessions";
import { statusBadgeClass } from "../../ui/styles";

const TONES: Record<SessionStatus, "active" | "succeeded" | "expired" | "revoked"> = {
  open: "active",
  closed: "succeeded",
  cancelled: "expired",
  failed: "revoked",
};

export function SessionStatusBadge(props: { status: SessionStatus }) {
  return <span class={statusBadgeClass(TONES[props.status])}>{props.status}</span>;
}
