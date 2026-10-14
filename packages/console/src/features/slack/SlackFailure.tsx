import { Show } from "solid-js";
import type { Failure } from "../../lib/error";
import { EmptyState } from "../../ui/QueryState";
import { Notice } from "../../ui/Notice";

function Details(props: { details: string | undefined }) {
  return (
    <Show when={props.details}>
      <details class="mt-1 text-chrome text-console-muted">
        <summary class="cursor-pointer font-mono">Technical details</summary>
        <pre class="mt-1.5 overflow-x-auto whitespace-pre-wrap break-words border border-console-border bg-console-bg-panel p-2 font-mono text-badge">{props.details}</pre>
      </details>
    </Show>
  );
}

// A failed Slack request: its message, with the server's text folded away.
export function SlackFailureNotice(props: { failure: Failure }) {
  return (
    <Notice tone="danger">
      <p class="m-0">{props.failure.message}</p>
      <Details details={props.failure.details} />
    </Notice>
  );
}

// The Slack integration is off on the control plane: what that means and who
// can change it, instead of an error to retry.
export function SlackNotSetUp(props: { failure: Failure; consequence: string }) {
  return (
    <EmptyState title="Slack is not set up for this Helmr installation." hint={`${props.consequence} The operator of this installation must enable the Slack integration on its control plane; nothing on this page can change that.`}>
      <Details details={props.failure.details} />
    </EmptyState>
  );
}
