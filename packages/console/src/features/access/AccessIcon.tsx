import type { JSX } from "solid-js";
import { Icon, type IconName } from "../../ui/Icon";

export type AccessKind = "member" | "group" | "all_members" | "api_key";

const KIND_ICON: Record<AccessKind, IconName> = { member: "member", group: "group", all_members: "all-members", api_key: "api-key" };
// What the icon says to assistive technology; a person or all members needs no word.
const KIND_TEXT: Record<AccessKind, string> = { member: "", group: " (group)", all_members: "", api_key: " (API key)" };

// A recipient or source: its kind's icon, then its name.
export function AccessLabel(props: { kind: AccessKind; children: JSX.Element }) {
  return (
    <span class="inline-flex min-w-0 items-center gap-1.5">
      <span class="text-console-subtle"><Icon name={KIND_ICON[props.kind]} /></span>
      <span class="truncate">{props.children}<span class="sr-only">{KIND_TEXT[props.kind]}</span></span>
    </span>
  );
}

// Marks a definition Helmr maintains: it cannot be renamed, edited or deleted.
export function ManagedMark(props: { title: string }) {
  return (
    <span class="inline-flex shrink-0 text-console-subtle" title={props.title}>
      <Icon name="lock" label={props.title} />
    </span>
  );
}
