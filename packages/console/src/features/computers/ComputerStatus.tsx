import { Show } from "solid-js";
import type { ComputerListItem } from "../../lib/computers";
import { StatusBadge } from "../../ui/StatusBadge";

// A Computer's status as the console shows it: its residency (whether and where
// it runs), or its lifecycle while it is being deleted or is deleted. An
// unavailable Computer's reason is the badge's tooltip.
export function ComputerStatus(props: { computer: Pick<ComputerListItem, "status" | "residency" | "error"> }) {
  const lifecycle = () => (props.computer.status === "available" ? undefined : props.computer.status);
  return (
    <Show
      when={lifecycle()}
      fallback={
        <span class="inline-flex" title={props.computer.residency === "unavailable" ? props.computer.error?.message : undefined}>
          <StatusBadge resource="computer_residency" status={props.computer.residency} />
        </span>
      }
    >
      {(status) => <StatusBadge resource="computer" status={status()} />}
    </Show>
  );
}
