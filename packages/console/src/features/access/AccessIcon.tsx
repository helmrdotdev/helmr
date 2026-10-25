import { Match, Switch, type JSX } from "solid-js";

export type AccessKind = "member" | "group" | "all_members" | "api_key";

function Icon(props: { children: JSX.Element; label?: string | undefined }) {
  return (
    <svg
      class="size-3.25 shrink-0"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      stroke-width="2"
      stroke-linecap="round"
      stroke-linejoin="round"
      role={props.label ? "img" : undefined}
      aria-label={props.label}
      aria-hidden={props.label ? undefined : "true"}
    >
      {props.children}
    </svg>
  );
}

// The kind of recipient an assignment names: a person, a group, everyone in the
// organization or an API key.
export function AccessKindIcon(props: { kind: AccessKind }) {
  return (
    <Icon>
      <Switch>
        <Match when={props.kind === "member"}>
          <circle cx="12" cy="8" r="4" />
          <path d="M4 21a8 8 0 0 1 16 0" />
        </Match>
        <Match when={props.kind === "group"}>
          <circle cx="9" cy="8" r="3.5" />
          <path d="M2 21a7 7 0 0 1 14 0M16 4.5a3.5 3.5 0 0 1 0 7M18 14.5a7 7 0 0 1 4 6.5" />
        </Match>
        <Match when={props.kind === "all_members"}>
          <rect x="4" y="3" width="16" height="18" rx="1" />
          <path d="M9 7h1M14 7h1M9 11h1M14 11h1M9 15h1M14 15h1M10 21v-3h4v3" />
        </Match>
        <Match when={props.kind === "api_key"}>
          <circle cx="8" cy="15" r="4" />
          <path d="m11 12 9-9M17 6l3 3" />
        </Match>
      </Switch>
    </Icon>
  );
}

// What the icon says to assistive technology; a person or all members needs no word.
const KIND_TEXT: Record<AccessKind, string> = { member: "", group: " (group)", all_members: "", api_key: " (API key)" };

// A recipient or source: its kind's icon, then its name.
export function AccessLabel(props: { kind: AccessKind; children: JSX.Element }) {
  return (
    <span class="inline-flex min-w-0 items-center gap-1.5">
      <span class="text-console-subtle"><AccessKindIcon kind={props.kind} /></span>
      <span class="truncate">{props.children}<span class="sr-only">{KIND_TEXT[props.kind]}</span></span>
    </span>
  );
}

// Marks a definition Helmr maintains: it cannot be renamed, edited or deleted.
export function ManagedMark(props: { title: string }) {
  return (
    <span class="inline-flex shrink-0 text-console-subtle" title={props.title}>
      <Icon label={props.title}>
        <rect x="5" y="11" width="14" height="10" rx="2" />
        <path d="M8 11V7a4 4 0 0 1 8 0v4" />
      </Icon>
    </span>
  );
}
