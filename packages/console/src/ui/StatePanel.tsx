import { Show, type JSX } from "solid-js";
import { ui } from "./styles";

type Props =
  | { loading: string }
  | { error: string }
  | { empty: string; hint?: JSX.Element; children?: JSX.Element };

export function StatePanel(props: Props) {
  if ("loading" in props) return <p class={ui.muted}>{props.loading}</p>;
  if ("error" in props) return <p class={ui.error} role="alert">{props.error}</p>;
  return (
    <div class={ui.emptyState}>
      <strong class="text-console-text">{props.empty}</strong>
      <Show when={props.hint}><span>{props.hint}</span></Show>
      {props.children}
    </div>
  );
}
