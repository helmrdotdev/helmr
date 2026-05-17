import { createUniqueId, type JSXElement, onMount } from "solid-js";
import { Portal } from "solid-js/web";
import { ui } from "./styles";

export function Modal(props: {
  title: string;
  onClose: () => void;
  children: JSXElement;
  closeDisabled?: boolean;
}) {
  const titleID = createUniqueId();
  let dialogRef: HTMLElement | undefined;

  const close = () => {
    if (props.closeDisabled) return;
    props.onClose();
  };

  onMount(() => {
    requestAnimationFrame(() => {
      const active = document.activeElement;
      if (active && dialogRef?.contains(active)) return;
      const autofocus = dialogRef?.querySelector<HTMLElement>("[autofocus]");
      const firstFocusable = dialogRef?.querySelector<HTMLElement>("button, input, textarea, select, a[href]");
      (autofocus ?? firstFocusable ?? dialogRef)?.focus();
    });
  });

  return (
    <Portal>
      <div
        class={ui.modalBackdrop}
        role="presentation"
        onMouseDown={(event) => {
          if (event.target === event.currentTarget) close();
        }}
      >
        <section
          ref={dialogRef}
          class={ui.modal}
          role="dialog"
          aria-modal="true"
          aria-labelledby={titleID}
          tabIndex={-1}
          onKeyDown={(event) => {
            if (event.key !== "Escape") return;
            event.preventDefault();
            event.stopPropagation();
            close();
          }}
        >
          <div class={ui.modalHeader}>
            <h2 id={titleID} class={ui.modalTitle}>{props.title}</h2>
            <button
              type="button"
              class={ui.modalCloseButton}
              disabled={props.closeDisabled}
              aria-label="Close modal"
              onClick={close}
            >
              <span class={ui.modalCloseIcon} aria-hidden="true" />
            </button>
          </div>
          <div class={ui.modalBody}>
            {props.children}
          </div>
        </section>
      </div>
    </Portal>
  );
}
