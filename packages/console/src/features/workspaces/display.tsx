import type { Workspace } from "../../lib/workspaces";
import { statusBadgeClass } from "../../ui/styles";

const TONES: Record<Workspace["status"], "succeeded" | "revoked" | "expired"> = {
  available: "succeeded",
  recovery_required: "revoked",
  deleting: "expired",
};

export function WorkspaceStatusBadge(props: { status: Workspace["status"] }) {
  return <span class={statusBadgeClass(TONES[props.status])}>{props.status}</span>;
}
