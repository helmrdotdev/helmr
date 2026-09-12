import { createSignal, onCleanup } from "solid-js";
import { cx } from "./styles";

const TAIL = 12;

export function formatID(value: string): string {
  const separator = value.indexOf(":");
  if (separator > 0 && separator < 12) {
    const digest = value.slice(separator + 1);
    return digest.length > TAIL ? `${value.slice(0, separator)}:…${digest.slice(-TAIL)}` : value;
  }
  return value.length > TAIL + 4 ? `…${value.slice(-TAIL)}` : value;
}

export function IDText(props: { value: string; full?: boolean; class?: string }) {
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

  return (
    <button
      type="button"
      class={cx(
        "inline-flex max-w-full cursor-copy items-center gap-1.5 border-0 bg-transparent p-0 text-left font-mono text-[11.5px] text-console-muted underline decoration-dotted decoration-console-faint underline-offset-3 transition hover:text-console-text",
        props.full && "break-all whitespace-normal",
        props.class,
      )}
      title={props.value}
      aria-label={`Copy ${props.value}`}
      onClick={() => void copy()}
    >
      <span>{props.full ? props.value : formatID(props.value)}</span>
      <span class={cx("text-[10px] text-console-accent", !copied() && "hidden")} aria-live="polite">copied</span>
    </button>
  );
}
