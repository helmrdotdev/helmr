import { A } from "@solidjs/router";
import { createSignal, onCleanup, Show } from "solid-js";
import { formatID } from "./id";
import { cx } from "./styles";

const TEXT = "font-mono text-[11.5px]";

export function IDText(props: { value: string; full?: boolean; href?: string | undefined; fallback?: string }) {
  const [copied, setCopied] = createSignal(false);
  let timer: number | undefined;
  onCleanup(() => window.clearTimeout(timer));

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(props.value);
      setCopied(true);
      window.clearTimeout(timer);
      timer = window.setTimeout(() => setCopied(false), 1200);
    } catch {
      setCopied(false);
    }
  };
  const display = () => (props.full ? props.value : formatID(props.value));
  const copyLabel = () => (copied() ? `Copied ${props.value}` : `Copy ${props.value}`);

  return (
    <Show when={props.value} fallback={<span class="text-console-faint">{props.fallback ?? "—"}</span>}>
      <Show
        when={props.href}
        fallback={
          <button
            type="button"
            class={cx(
              TEXT,
              "inline-flex max-w-full cursor-copy items-center gap-1.5 border-0 bg-transparent p-0 text-left text-console-muted underline decoration-dotted decoration-console-faint underline-offset-3 transition hover:text-console-text",
              props.full && "break-all whitespace-normal",
            )}
            title={props.value}
            aria-label={copyLabel()}
            onClick={() => void copy()}
          >
            <span>{display()}</span>
            <span class="text-[10px] text-console-accent" aria-hidden="true">{copied() ? "copied" : ""}</span>
          </button>
        }
      >
        {(href) => (
          <span class={cx("inline-flex max-w-full items-center gap-1.5", props.full && "break-all whitespace-normal")}>
            <A href={href()} class={cx(TEXT, "text-console-accent hover:text-console-accent-hover")} title={props.value}>
              {display()}
            </A>
            <button
              type="button"
              class="inline-grid size-4 shrink-0 cursor-copy place-items-center rounded-xs border border-transparent bg-transparent p-0 font-mono text-[10px] leading-none text-console-subtle transition hover:border-console-border hover:text-console-text"
              title="Copy"
              aria-label={copyLabel()}
              onClick={() => void copy()}
            >
              <span aria-hidden="true">{copied() ? "✓" : "⧉"}</span>
            </button>
          </span>
        )}
      </Show>
    </Show>
  );
}
