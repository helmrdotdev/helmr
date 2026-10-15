import { Match, Show, Switch, untrack, type Accessor, type JSX } from "solid-js";
import { A } from "@solidjs/router";
import { ApiError } from "../lib/api";
import { errorDetails } from "../lib/error";
import { formatID } from "./id";
import { Notice } from "./Notice";
import { cx, ui } from "./styles";

// What is missing and the next step; may hold a CodeBlock or an action. Dashed
// and tinted directly on the page; inside a container body it drops both
// (`ui.containerBody`).
export function EmptyState(props: { title: string; hint?: JSX.Element; children?: JSX.Element; class?: string }) {
  return (
    <div class={cx(ui.emptyState, props.class)} data-empty-state>
      <strong class="font-medium text-console-text">{props.title}</strong>
      <Show when={props.hint}><span>{props.hint}</span></Show>
      {props.children}
    </div>
  );
}

// A failed read: the page's sentence in danger text, a compact Retry, and the
// server's own code and message folded under "Technical details".
export function ErrorState(props: { message: string; details?: string | undefined; onRetry?: () => void }) {
  return (
    <div class="flex flex-col gap-1.5" role="alert">
      <div class="flex flex-wrap items-center gap-2">
        <p class="m-0 text-body font-medium text-console-danger">{props.message}</p>
        <Show when={props.onRetry}>
          {(retry) => <button type="button" class={ui.compactButton} onClick={() => retry()()}>Retry</button>}
        </Show>
      </div>
      <TechnicalDetails details={props.details} />
    </div>
  );
}

// The server's code and message for support, collapsed behind a text toggle.
export function TechnicalDetails(props: { details: string | undefined }) {
  return (
    <Show when={props.details}>
      <details class="text-chrome text-console-muted">
        <summary class="cursor-pointer font-mono">Technical details</summary>
        <p class="m-0 mt-1 whitespace-pre-wrap break-words font-mono text-badge">{props.details}</p>
      </details>
    </Show>
  );
}

// A read the account is not allowed to make: a neutral sentence and no Retry,
// since retrying cannot change the answer.
export function DeniedState(props: { message: string }) {
  return <Notice tone="info">{props.message}</Notice>;
}

// A resource that does not exist in the selected Environment: what was asked
// for, where it was looked for, and the way back. The page titles itself
// "<Resource> not found" and drops its status and actions.
export function NotFoundState(props: { noun: string; id?: string | undefined; environment?: string | undefined; back: { href: string; label: string } }) {
  return (
    <div class="flex flex-col items-start gap-3">
      <p class={cx(ui.muted, "m-0")}>
        No {props.noun}
        <Show when={props.id}>{(id) => <> <span class="font-mono text-console-text" title={id()}>{formatID(id())}</span></>}</Show>
        <Show when={props.environment}>{(environment) => <> in {environment()}</>}</Show>
        . It may be in another environment, or the link is wrong.
      </p>
      <A href={props.back.href} class={ui.secondaryButton}>{props.back.label}</A>
    </div>
  );
}

// Whether a failed read means the resource is not there.
export function isNotFound(error: unknown): boolean {
  return error instanceof ApiError && error.status === 404;
}

// Whether a failed read was refused for this account.
export function isDenied(error: unknown): boolean {
  return error instanceof ApiError && error.status === 403;
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

// One query's pending, error (with Retry), denied, not found, empty and content
// states, so pages do not nest them by hand. `error` is the page's sentence for
// a failed read (lib/error `loadError`); the server's text only ever appears
// under technical details. A 403 shows that sentence as a denial without Retry;
// `notFound` replaces the error when `missing` (default: a 404) holds. `empty` decides when loaded data counts as empty. The
// content receives the data as an accessor, so a refetch updates it in place
// instead of rebuilding it. Once data has loaded, a failed refetch keeps the
// content and shows a notice with Retry above it, and a refetch never blanks
// it. `pending` is the content's skeleton (ui/Skeleton) for the first load;
// without it nothing renders.
export function QueryState<T>(props: {
  query: QueryLike<T>;
  pending?: JSX.Element;
  error: (error: unknown) => string;
  notFound?: JSX.Element;
  missing?: (error: unknown) => boolean;
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
          action={<button type="button" class={ui.compactButton} onClick={retry}>Retry</button>}
        >
          {props.error(props.query.error)}
        </Notice>
      </Show>
      <Switch>
        <Match when={!loaded() && props.query.isError && props.notFound && (props.missing ?? isNotFound)(props.query.error)}>
          {props.notFound}
        </Match>
        <Match when={!loaded() && props.query.isError && isDenied(props.query.error)}>
          <DeniedState message={props.error(props.query.error)} />
        </Match>
        <Match when={!loaded() && props.query.isError}>
          <ErrorState message={props.error(props.query.error)} details={errorDetails(props.query.error)} onRetry={retry} />
        </Match>
        <Match when={!loaded()}>
          {props.pending}
        </Match>
        <Match when={props.empty && props.query.data !== undefined && props.empty.when(props.query.data)}>
          <EmptyState title={props.empty!.title} hint={props.empty!.hint}>{props.empty!.action}</EmptyState>
        </Match>
        <Match when={loaded()}>{untrack(() => props.children(data))}</Match>
      </Switch>
    </>
  );
}
