import { Match, Show, Switch, type JSX } from "solid-js";
import { cx, ui } from "./styles";

// What is missing and the next step; may hold a CodeBlock or an action.
export function EmptyState(props: { title: string; hint?: JSX.Element; children?: JSX.Element; class?: string }) {
  return (
    <div class={cx(ui.emptyState, props.class)}>
      <strong class="font-medium text-console-text">{props.title}</strong>
      <Show when={props.hint}><span>{props.hint}</span></Show>
      {props.children}
    </div>
  );
}

export function PendingState(props: { children: JSX.Element }) {
  return <p class={ui.muted}>{props.children}</p>;
}

export function ErrorState(props: { message: string; onRetry?: () => void }) {
  return (
    <div class="mt-3 flex flex-wrap items-center gap-2" role="alert">
      <p class="m-0 text-body font-medium text-console-danger">{props.message}</p>
      <Show when={props.onRetry}>
        {(retry) => <button type="button" class={ui.secondaryButton} onClick={() => retry()()}>Retry</button>}
      </Show>
    </div>
  );
}

type QueryLike<T> = {
  isPending: boolean;
  isError: boolean;
  error: unknown;
  data: T | undefined;
  refetch: () => unknown;
};

// One query's pending, error (with Retry), empty and content states, so pages do
// not nest them by hand. `empty` decides when loaded data counts as empty.
export function QueryState<T>(props: {
  query: QueryLike<T>;
  pending: string;
  error: (error: unknown) => string;
  empty?: { when: (data: T) => boolean; title: string; hint?: JSX.Element; action?: JSX.Element };
  children: (data: T) => JSX.Element;
}) {
  return (
    <Switch>
      <Match when={props.query.isError}>
        <ErrorState message={props.error(props.query.error)} onRetry={() => void props.query.refetch()} />
      </Match>
      <Match when={props.query.isPending}>
        <PendingState>{props.pending}</PendingState>
      </Match>
      <Match when={props.empty && props.query.data !== undefined && props.empty.when(props.query.data)}>
        <EmptyState title={props.empty!.title} hint={props.empty!.hint}>{props.empty!.action}</EmptyState>
      </Match>
      <Match when={props.query.data !== undefined}>{props.children(props.query.data as T)}</Match>
    </Switch>
  );
}
