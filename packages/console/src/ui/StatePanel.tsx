import type { JSX } from "solid-js";
import { EmptyState, PendingState } from "./QueryState";
import { ui } from "./styles";

type Props =
  | { loading: string }
  | { error: string }
  | { empty: string; hint?: JSX.Element; children?: JSX.Element };

// A single loading, error or empty line for states a page computes itself. Query
// results use QueryState instead.
export function StatePanel(props: Props) {
  if ("loading" in props) return <PendingState>{props.loading}</PendingState>;
  if ("error" in props) return <p class={ui.error} role="alert">{props.error}</p>;
  return <EmptyState title={props.empty} hint={props.hint}>{props.children}</EmptyState>;
}
