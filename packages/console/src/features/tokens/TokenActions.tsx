import { createSignal, Show } from "solid-js";
import { ApiError } from "../../lib/api";
import { cancelToken, completeToken, type TokenListItem } from "../../lib/tokens";
import { ConfirmModal } from "../../ui/ConfirmModal";
import { Modal } from "../../ui/Modal";
import { ui } from "../../ui/styles";
import { TagList } from "../../ui/TagList";

// List rows only carry tags; the detail page passes the full Token with metadata.
type ActionTarget = TokenListItem & { metadata?: unknown };

export function tokenActionErrorMessage(error: unknown, fallback: string): string {
  if (error instanceof ApiError && error.code === "forbidden") return "You do not have permission to do this.";
  if (error instanceof ApiError) return error.message;
  return fallback;
}

function TokenSummary(props: { token: ActionTarget }) {
  return (
    <dl class="mt-2.5 grid grid-cols-[auto_minmax(0,1fr)] gap-x-3 gap-y-1.5 border border-console-border-strong bg-console-bg-panel px-3 py-2.5 text-[12px]">
      <dt class="font-mono text-[10.5px] font-medium uppercase tracking-[0.04em] text-console-subtle">Token</dt>
      <dd class="m-0 break-all font-mono text-[11.5px] text-console-text">{props.token.id}</dd>
      <dt class="font-mono text-[10.5px] font-medium uppercase tracking-[0.04em] text-console-subtle">Tags</dt>
      <dd class="m-0"><TagList tags={props.token.tags} /></dd>
      <Show when={props.token.metadata !== undefined}>
        <dt class="font-mono text-[10.5px] font-medium uppercase tracking-[0.04em] text-console-subtle">Metadata</dt>
        <dd class="m-0 min-w-0">
          <pre class="m-0 max-h-32 overflow-auto whitespace-pre-wrap break-words font-mono text-[11px] leading-normal text-console-text">
            {JSON.stringify(props.token.metadata ?? null, null, 2)}
          </pre>
        </dd>
      </Show>
    </dl>
  );
}

export function CompleteTokenModal(props: {
  token: ActionTarget;
  projectID: string;
  environmentID: string;
  onClose: () => void;
  onCompleted: () => Promise<void>;
}) {
  const [result, setResult] = createSignal("{}");
  const [submitting, setSubmitting] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  const submit = async (event: Event) => {
    event.preventDefault();
    let parsed: unknown;
    try {
      parsed = JSON.parse(result());
    } catch {
      setError("Result must be valid JSON.");
      return;
    }
    setError(null);
    setSubmitting(true);
    try {
      await completeToken(props.token.id, { projectID: props.projectID, environmentID: props.environmentID }, {
        result: parsed,
        idempotency_key: crypto.randomUUID(),
      });
      await props.onCompleted();
      props.onClose();
    } catch (cause) {
      setError(tokenActionErrorMessage(cause, "Could not complete this Token."));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Modal title="Complete Token" onClose={props.onClose} closeDisabled={submitting()}>
      <form onSubmit={submit}>
        <div class={ui.modalIntro}>
          The result is delivered to the waiting Run as JSON.
          <TokenSummary token={props.token} />
        </div>
        <label class={ui.field}>
          <span>Result (JSON)</span>
          <textarea
            class={`${ui.textarea} font-mono`}
            rows={6}
            value={result()}
            onInput={(event) => setResult(event.currentTarget.value)}
            spellcheck={false}
            autofocus
          />
        </label>
        <Show when={error()}>
          <p class={ui.fieldError} role="alert">{error()}</p>
        </Show>
        <div class={ui.modalActions}>
          <button type="button" class={ui.secondaryButton} disabled={submitting()} onClick={props.onClose}>
            Cancel
          </button>
          <button type="submit" class={ui.button} disabled={submitting()}>
            {submitting() ? "Completing..." : "Complete"}
          </button>
        </div>
      </form>
    </Modal>
  );
}

export function CancelTokenModal(props: {
  token: ActionTarget;
  projectID: string;
  environmentID: string;
  onClose: () => void;
  onCancelled: () => Promise<void>;
}) {
  return (
    <ConfirmModal
      title="Cancel Token"
      confirmLabel="Cancel Token"
      busyLabel="Cancelling..."
      tone="danger"
      onClose={props.onClose}
      onConfirm={async () => {
        await cancelToken(
          props.token.id,
          { projectID: props.projectID, environmentID: props.environmentID },
          { idempotency_key: crypto.randomUUID() },
        );
        await props.onCancelled();
      }}
      errorMessage={(error) => tokenActionErrorMessage(error, "Could not cancel this Token.")}
    >
      The waiting Run receives a cancelled Token and cannot be completed later.
      <TokenSummary token={props.token} />
    </ConfirmModal>
  );
}
