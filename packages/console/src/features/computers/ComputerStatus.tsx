import { Show } from "solid-js";
import type { ComputerListItem } from "../../lib/computers";
import { StatusBadge } from "../../ui/StatusBadge";

// A Computer's status as the console shows it: its residency (whether and where
// it runs), or its lifecycle while it is being deleted or is deleted. An
// unavailable Computer's reason is the badge's tooltip.
export function ComputerStatus(props: { computer: Pick<ComputerListItem, "status" | "residency" | "error" | "execution_wait"> }) {
  const lifecycle = () => (props.computer.status === "available" ? undefined : props.computer.status);
  const waitLabel = () => ({ scheduling: "Waiting for capacity", environment_limit: "Environment limit", saving: "Saving" })[props.computer.execution_wait?.reason ?? "scheduling"];
  return (
    <Show
      when={lifecycle()}
      fallback={
        <span class="inline-flex items-center gap-2" title={props.computer.residency === "unavailable" ? props.computer.error?.message : undefined}>
          <StatusBadge resource="computer_residency" status={props.computer.residency} />
          <Show when={props.computer.execution_wait}><span class="text-xs text-muted-foreground">{waitLabel()}</span></Show>
        </span>
      }
    >
      {(status) => <StatusBadge resource="computer" status={status()} />}
    </Show>
  );
}
