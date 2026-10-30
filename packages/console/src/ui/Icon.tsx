import { Match, Switch } from "solid-js";
import { cx } from "./styles";

export type IconName =
  | "chevron-right" | "chevron-down" | "check" | "cross" | "arrow-down"
  | "member" | "group" | "all-members" | "api-key" | "lock"
  | "slack-channel" | "slack-dm" | "terminal" | "held" | "warning";

// The console's icons: 13px strokes in the current text colour. Decorative by
// default; pass `label` when the icon carries meaning no nearby word gives.
export function Icon(props: { name: IconName; label?: string | undefined; class?: string | undefined }) {
  return (
    <svg
      class={cx("size-3.25 shrink-0", props.class)}
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
      <Switch>
        <Match when={props.name === "chevron-right"}><path d="m9 6 6 6-6 6" /></Match>
        <Match when={props.name === "chevron-down"}><path d="m6 9 6 6 6-6" /></Match>
        <Match when={props.name === "check"}><path d="m5 12 5 5 9-10" /></Match>
        <Match when={props.name === "cross"}><path d="M7 7l10 10M17 7 7 17" /></Match>
        <Match when={props.name === "arrow-down"}><path d="M12 5v14M6 13l6 6 6-6" /></Match>
        <Match when={props.name === "member"}><circle cx="12" cy="8" r="4" /><path d="M4 21a8 8 0 0 1 16 0" /></Match>
        <Match when={props.name === "group"}><circle cx="9" cy="8" r="3.5" /><path d="M2 21a7 7 0 0 1 14 0M16 4.5a3.5 3.5 0 0 1 0 7M18 14.5a7 7 0 0 1 4 6.5" /></Match>
        <Match when={props.name === "all-members"}><rect x="4" y="3" width="16" height="18" rx="1" /><path d="M9 7h1M14 7h1M9 11h1M14 11h1M9 15h1M14 15h1M10 21v-3h4v3" /></Match>
        <Match when={props.name === "api-key"}><circle cx="8" cy="15" r="4" /><path d="m11 12 9-9M17 6l3 3" /></Match>
        <Match when={props.name === "lock"}><rect x="5" y="11" width="14" height="10" rx="2" /><path d="M8 11V7a4 4 0 0 1 8 0v4" /></Match>
        <Match when={props.name === "slack-channel"}><path d="M4 9h16M4 15h16M10 3 8 21M16 3l-2 18" /></Match>
        <Match when={props.name === "slack-dm"}><path d="M21 12a8 8 0 0 1-11.6 7.1L4 20l1-4.6A8 8 0 1 1 21 12z" /></Match>
        <Match when={props.name === "terminal"}><path d="m5 8 4 4-4 4M12 17h7" /></Match>
        <Match when={props.name === "held"}><path d="M9 5v14M15 5v14" /></Match>
        <Match when={props.name === "warning"}><path d="M12 3 2 20h20zM12 10v4M12 17h.01" /></Match>
      </Switch>
    </svg>
  );
}
