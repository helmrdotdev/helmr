import { Icon } from "../../ui/Icon";

// Work in a Session waits for a person.
export function HeldLabel(props: { title?: string | undefined }) {
  return (
    <span class="inline-flex items-center gap-1 text-console-warning" title={props.title}>
      <Icon name="held" />
      <span>Held</span>
    </span>
  );
}
