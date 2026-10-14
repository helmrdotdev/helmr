import { Show, type JSX } from "solid-js";
import { cx, toneClass, ui, type Tone } from "./styles";

type NoticeTone = Exclude<Tone, "neutral">;

// A message about the page or a task: info, warning, danger or success. Danger
// notices are announced as alerts, the others as status; pass `role="none"` when an
// enclosing live region already announces it. With `onDismiss` it gets a close
// control; `action` adds one inline link or button. The `bar` variant spans the
// page under the app bar.
export function Notice(props: {
  tone: NoticeTone;
  children: JSX.Element;
  action?: JSX.Element;
  onDismiss?: () => void;
  variant?: "inline" | "bar";
  role?: "status" | "alert" | "none";
  class?: string;
}) {
  const role = () => (props.role === "none" ? undefined : props.role ?? (props.tone === "danger" ? "alert" : "status"));
  return (
    <div class={cx(props.variant === "bar" ? ui.noticeBar : ui.notice, toneClass[props.tone], props.class)} role={role()}>
      <div class="min-w-0 flex-1">{props.children}</div>
      <Show when={props.action}>
        <div class="shrink-0 font-mono text-chrome">{props.action}</div>
      </Show>
      <Show when={props.onDismiss}>
        {(dismiss) => (
          <button type="button" class={ui.closeButton} aria-label="Dismiss notice" onClick={() => dismiss()()}>
            <span class={ui.closeIcon} aria-hidden="true" />
          </button>
        )}
      </Show>
    </div>
  );
}

const INLINE_TONE: Record<Tone, string> = {
  info: "text-console-info",
  warning: "text-console-warning",
  success: "text-console-success",
  danger: "text-console-danger",
  neutral: "text-console-muted",
};

// A short transient result next to the control that caused it ("Saved", "Copied").
export function InlineStatus(props: { tone?: Tone; children: JSX.Element; class?: string }) {
  return (
    <span class={cx("font-mono text-chrome", INLINE_TONE[props.tone ?? "success"], props.class)} role="status">
      {props.children}
    </span>
  );
}
