import { Match, Show, Switch, untrack, type Accessor, type JSX } from "solid-js";
import { Notice } from "./Notice";
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
  // Infinite queries: the failure was loading the next page, which the
  // LoadMore footer reports instead.
  isFetchNextPageError?: boolean;
};

// One query's pending, error (with Retry), empty and content states, so pages do
// not nest them by hand. `empty` decides when loaded data counts as empty. The
// content receives the data as an accessor, so a refetch updates it in place
// instead of rebuilding it. Once data has loaded, a failed refetch keeps the
// content and shows a notice with Retry above it; without `pending` nothing
// renders while the first load runs.
export function QueryState<T>(props: {
  query: QueryLike<T>;
  pending?: string;
  error: (error: unknown) => string;
  empty?: { when: (data: T) => boolean; title: string; hint?: JSX.Element; action?: JSX.Element };
  children: (data: Accessor<T>) => JSX.Element;
}) {
  // The last loaded value, so content being torn down never reads a missing one.
  let last: T;
  const loaded = () => props.query.data !== undefined;
  const data = (): T => {
    if (props.query.data !== undefined) last = props.query.data;
    return last;
  };
  const retry = () => void props.query.refetch();
  return (
    <>
      <Show when={loaded() && props.query.isError && !props.query.isFetchNextPageError}>
        <Notice
          tone="danger"
          class="mb-3"
          action={<button type="button" class={cx(ui.secondaryButton, "h-6 min-h-6 px-2")} onClick={retry}>Retry</button>}
        >
          {props.error(props.query.error)}
        </Notice>
      </Show>
      <Switch>
        <Match when={!loaded() && props.query.isError}>
          <ErrorState message={props.error(props.query.error)} onRetry={retry} />
        </Match>
        <Match when={!loaded()}>
          <Show when={props.pending}>{(pending) => <PendingState>{pending()}</PendingState>}</Show>
        </Match>
        <Match when={props.empty && props.query.data !== undefined && props.empty.when(props.query.data)}>
          <EmptyState title={props.empty!.title} hint={props.empty!.hint}>{props.empty!.action}</EmptyState>
        </Match>
        <Match when={loaded()}>{untrack(() => props.children(data))}</Match>
      </Switch>
    </>
  );
}
