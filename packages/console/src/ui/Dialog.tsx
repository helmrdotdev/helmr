import { createSignal, createUniqueId, onCleanup, onMount, Show, type JSX } from "solid-js";
import { Portal } from "solid-js/web";
import { errorMessage } from "../lib/error";
import { EnvChip } from "./EnvChip";
import { FormActions } from "./Field";
import { openDismissLayer } from "./dismiss";
import { focusableIn, focusReturnTarget } from "./Popover";
import { Notice } from "./Notice";
import { ContainerHeader } from "./Panel";
import { cx, ui } from "./styles";
import { useClipboard } from "./useClipboard";

// A modal container: the container header with a close control, closed by Escape, the close
// control or a press on the backdrop (unless `closeDisabled`). Focus moves to the
// first `autofocus` element, or the first control, stays inside while open (Tab
// cycles), and returns on close to the control that opened it; for a dialog opened
// from a menu item that is the menu's trigger.
export function Dialog(props: {
  title: string;
  onClose: () => void;
  children: JSX.Element;
  closeDisabled?: boolean;
  class?: string;
  // Replaces the body's padding, for content that draws its own edges.
  bodyClass?: string;
}) {
  const titleID = createUniqueId();
  let dialogRef: HTMLElement | undefined;
  const opener = typeof document === "undefined" ? null : focusReturnTarget(document.activeElement);

  const close = () => {
    if (props.closeDisabled) return;
    props.onClose();
  };

  const trapTab = (event: KeyboardEvent) => {
    if (!dialogRef) return;
    const controls = focusableIn(dialogRef);
    const first = controls[0];
    const last = controls[controls.length - 1];
    if (!first || !last) {
      event.preventDefault();
      return;
    }
    const active = document.activeElement;
    const outside = !active || !dialogRef.contains(active) || active === dialogRef;
    if (event.shiftKey && (outside || active === first)) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && (outside || active === last)) {
      event.preventDefault();
      first.focus();
    }
  };

  onMount(() => {
    // Escape closes the dialog wherever focus is, after any popover opened above it.
    onCleanup(openDismissLayer({
      contains: (target) => !!dialogRef?.contains(target),
      dismiss: (reason) => {
        if (reason === "escape") close();
      },
    }));
    requestAnimationFrame(() => {
      const active = document.activeElement;
      if (active && dialogRef?.contains(active)) return;
      const autofocus = dialogRef?.querySelector<HTMLElement>("[autofocus]");
      const firstFocusable = dialogRef?.querySelector<HTMLElement>("input, textarea, select, button:not([data-dialog-close]), a[href]");
      (autofocus ?? firstFocusable ?? dialogRef)?.focus();
    });
  });
  onCleanup(() => {
    if (opener?.isConnected) opener.focus();
  });

  return (
    <Portal>
      <div
        class="fixed inset-0 z-50 grid place-items-center bg-slate-950/30 p-3 backdrop-blur-[2px]"
        role="presentation"
        onMouseDown={(event) => {
          if (event.target === event.currentTarget) close();
        }}
      >
        <section
          ref={dialogRef}
          class={cx(ui.container, "flex max-h-[min(82vh,620px)] w-full max-w-115 flex-col overflow-hidden outline-none", props.class)}
          role="dialog"
          aria-modal="true"
          aria-labelledby={titleID}
          tabIndex={-1}
          onKeyDown={(event) => {
            if (event.defaultPrevented) return;
            if (event.key === "Tab") trapTab(event);
          }}
        >
          <ContainerHeader
            id={titleID}
            title={props.title}
            actions={<button
              type="button"
              class={ui.closeButton}
              disabled={props.closeDisabled}
              aria-label="Close modal"
              data-dialog-close
              onClick={close}
            >
              <span class={ui.closeIcon} aria-hidden="true" />
            </button>}
          />
          <div class={cx("min-h-0 overflow-y-auto", props.bodyClass ?? "p-3")}>{props.children}</div>
        </section>
      </div>
    </Portal>
  );
}

// A short create or edit form in a dialog: optional intro, fields, error, then
// Cancel and the primary action.
export function FormDialog(props: {
  title: string;
  intro?: JSX.Element;
  submitLabel: string;
  busyLabel: string;
  busy: boolean;
  disabled?: boolean | undefined;
  error?: string | null | undefined;
  onSubmit: () => void;
  onClose: () => void;
  children: JSX.Element;
}) {
  return (
    <Dialog title={props.title} onClose={props.onClose} closeDisabled={props.busy} bodyClass="flex flex-col">
      <form
        class="flex min-h-0 flex-1 flex-col"
        onSubmit={(event) => {
          event.preventDefault();
          props.onSubmit();
        }}
      >
        <div class="min-h-0 overflow-y-auto p-3">
          <Show when={props.intro}><p class={ui.modalIntro}>{props.intro}</p></Show>
          {props.children}
        </div>
        <Show when={props.error}><p class="m-0 shrink-0 border-t border-console-border-soft px-3 py-2 text-body font-medium text-console-danger" role="alert">{props.error}</p></Show>
        <FormActions class={ui.dialogFooter} submitLabel={props.submitLabel} busyLabel={props.busyLabel} busy={props.busy} disabled={props.disabled} onCancel={props.onClose} />
      </form>
    </Dialog>
  );
}

// Every confirmation. It names the target and, when the action is scoped to one,
// its Environment; the danger tone marks destructive actions. The dialog closes
// when `onConfirm` resolves and shows the error when it rejects.
export function ConfirmDialog(props: {
  title: string;
  children: JSX.Element;
  confirmLabel: string;
  busyLabel?: string;
  tone?: "default" | "danger";
  target?: JSX.Element;
  environment?: { name: string; colorHex: string | undefined; protected?: boolean | undefined } | undefined;
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
      setError(props.errorMessage?.(cause) ?? errorMessage(cause));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog title={props.title} onClose={props.onClose} closeDisabled={busy()} bodyClass="flex flex-col">
      <div class="min-h-0 overflow-y-auto p-3">
        <Show when={props.target || props.environment}>
          <p class="mb-2 flex flex-wrap items-center gap-1.5 font-mono text-chrome text-console-text">
            {props.target}
            <Show when={props.environment}>
              {(environment) => (
                <>
                  <Show when={props.target}><span class="text-console-subtle">in</span></Show>
                  <EnvChip name={environment().name} colorHex={environment().colorHex} protected={environment().protected} />
                </>
              )}
            </Show>
          </p>
        </Show>
        <div class="text-body leading-normal text-console-muted">{props.children}</div>
      </div>
      <Show when={error()}>
        <p class="m-0 shrink-0 border-t border-console-border-soft px-3 py-2 text-body font-medium text-console-danger" role="alert">{error()}</p>
      </Show>
      <div class={ui.dialogFooter}>
        <button type="button" class={ui.secondaryButton} disabled={busy()} onClick={props.onClose}>
          Cancel
        </button>
        <button
          type="button"
          class={props.tone === "danger" ? ui.dangerOutlineButton : ui.button}
          disabled={busy()}
          onClick={() => void confirm()}
          autofocus
        >
          {busy() ? props.busyLabel ?? props.confirmLabel : props.confirmLabel}
        </button>
      </div>
    </Dialog>
  );
}

// A value shown once (API key, enrollment token) with Copy and a
// warning that it is not shown again.
export function RevealDialog(props: {
  title: string;
  value: string;
  intro?: JSX.Element;
  warning?: string;
  doneLabel?: string;
  onClose: () => void;
}) {
  const clipboard = useClipboard();
  return (
    <Dialog title={props.title} onClose={props.onClose}>
      <div class={ui.blockStack}>
        <Show when={props.intro}><p class="m-0 text-body leading-normal text-console-muted">{props.intro}</p></Show>
        <Notice tone="warning">{props.warning ?? "Copy this value now. It is not shown again."}</Notice>
        <div class="flex border border-console-border-strong">
          <code class="min-w-0 flex-1 select-all overflow-x-auto whitespace-nowrap bg-console-bg-panel px-2.5 py-2 font-mono text-chrome text-console-text">{props.value}</code>
          <button
            type="button"
            class="cursor-pointer border-0 border-l border-console-border-strong gradient-button px-3 font-mono text-chrome font-medium text-console-text hover:gradient-button-hover"
            onClick={() => void clipboard.copy(props.value)}
          >
            {clipboard.state() === "copied" ? "Copied" : clipboard.state() === "failed" ? "Could not copy" : "Copy"}
          </button>
          <clipboard.LiveRegion />
        </div>
      </div>
      <div class={ui.modalActions}>
        <button type="button" class={ui.button} onClick={props.onClose}>{props.doneLabel ?? "Done"}</button>
      </div>
    </Dialog>
  );
}
