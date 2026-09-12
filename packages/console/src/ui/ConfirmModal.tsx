import { createSignal, Show, type JSXElement } from "solid-js";
import { Modal } from "./Modal";
import { cx, ui } from "./styles";

export function ConfirmModal(props: {
  title: string;
  children: JSXElement;
  confirmLabel: string;
  busyLabel?: string;
  tone?: "default" | "danger";
  onConfirm: () => Promise<void>;
  onClose: () => void;
  errorMessage?: (error: unknown) => string;
}) {
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  const confirm = async () => {
    setBusy(true);
    setError(null);
    try {
      await props.onConfirm();
      props.onClose();
    } catch (cause) {
      setError(props.errorMessage?.(cause) ?? (cause instanceof Error ? cause.message : "Something went wrong."));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal title={props.title} onClose={props.onClose} closeDisabled={busy()}>
      <div class={ui.modalIntro}>{props.children}</div>
      <Show when={error()}>
        <p class={ui.error} role="alert">{error()}</p>
      </Show>
      <div class={ui.modalActions}>
        <button type="button" class={ui.secondaryButton} disabled={busy()} onClick={props.onClose}>
          Keep
        </button>
        <button
          type="button"
          class={cx(props.tone === "danger" ? ui.dangerOutlineButton : ui.button)}
          disabled={busy()}
          onClick={() => void confirm()}
          autofocus
        >
          {busy() ? props.busyLabel ?? props.confirmLabel : props.confirmLabel}
        </button>
      </div>
    </Modal>
  );
}
