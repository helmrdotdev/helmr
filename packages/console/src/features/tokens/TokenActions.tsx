import { createSignal, Show } from "solid-js";
import { ApiError } from "../../lib/api";
import { cancelToken, completeToken, type TokenListItem } from "../../lib/tokens";
import { ConfirmModal } from "../../ui/ConfirmModal";
import { IDText } from "../../ui/IDText";
import { Modal } from "../../ui/Modal";
import { ui } from "../../ui/styles";

export function tokenActionErrorMessage(error: unknown, fallback: string): string {
  if (error instanceof ApiError && error.code === "forbidden") return "You do not have permission to do this.";
  if (error instanceof ApiError) return error.message;
  return fallback;
}

export function CompleteTokenModal(props: {
  token: TokenListItem;
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
        <p class={ui.modalIntro}>
          The result is delivered to the waiting Run as JSON. Token <IDText value={props.token.id} />.
        </p>
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
  token: TokenListItem;
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
      The waiting Run receives a cancelled Token and cannot be completed later. Token <IDText value={props.token.id} />.
    </ConfirmModal>
  );
}
