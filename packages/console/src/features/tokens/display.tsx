import type { TokenStatus } from "../../lib/tokens";
import { statusBadgeClass } from "../../ui/styles";

const TONES: Record<TokenStatus, "waiting" | "succeeded" | "expired" | "revoked"> = {
  pending: "waiting",
  completed: "succeeded",
  expired: "expired",
  cancelled: "revoked",
};

export function TokenStatusBadge(props: { status: TokenStatus }) {
  return <span class={statusBadgeClass(TONES[props.status])}>{props.status}</span>;
}
