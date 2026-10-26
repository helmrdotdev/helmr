import { Show } from "solid-js";
import type { Failure } from "../../lib/error";
import { Notice } from "../../ui/Notice";

function Details(props: { details: string | undefined }) {
  return (
    <Show when={props.details}>
      <details class="mt-1 text-chrome text-console-muted">
        <summary class="cursor-pointer font-mono">Technical details</summary>
        <p class="m-0 mt-1 whitespace-pre-wrap break-words font-mono text-badge">{props.details}</p>
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
// can change it, as plain text in its panel instead of an error to retry.
export function SlackNotSetUp(props: { failure: Failure; consequence: string }) {
  return (
    <div>
      <p class="m-0 text-body">Slack is not set up in Helmr. {props.consequence}</p>
      <p class="m-0 text-body text-console-muted">The operator enables it on the control plane; nothing on this page can change that.</p>
      <Details details={props.failure.details} />
    </div>
  );
}
