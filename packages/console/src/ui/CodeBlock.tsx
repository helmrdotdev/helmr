import { Show } from "solid-js";
import { cx, ui } from "./styles";
import { useClipboard } from "./useClipboard";

// Every block of code or output (JSON, stdout, CLI snippets), with an optional
// Copy button for things meant to be pasted.
export function CodeBlock(props: { code: string; copy?: boolean; label?: string; class?: string }) {
  const clipboard = useClipboard();
  return (
    <div class={cx("relative", props.class)}>
      <pre class={cx(ui.codeBlock, props.copy && "pr-18")} aria-label={props.label}>{props.code}</pre>
      <Show when={props.copy}>
        <button
          type="button"
          class={cx(ui.compactButton, "absolute right-2 top-2")}
          onClick={() => void clipboard.copy(props.code)}
        >
          {clipboard.state() === "copied" ? "Copied" : clipboard.state() === "failed" ? "Could not copy" : "Copy"}
        </button>
        <clipboard.LiveRegion />
      </Show>
    </div>
  );
}
