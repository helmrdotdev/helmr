import { A } from "@solidjs/router";
import { createSignal, onCleanup, Show } from "solid-js";
import { formatID } from "./id";
import { cx } from "./styles";

const TEXT = "font-mono text-[11.5px]";

type Mode = "short" | "link" | "full";

type Props =
  // Short form as plain text, full value in `title`. List rows and inline references.
  | { value: string; mode?: "short"; fallback?: string }
  // Short form as a link to the resource; falls back to short text without an href.
  | { value: string; mode: "link"; href: string | undefined; fallback?: string }
  // Full value once, and the text itself copies on click. Detail page headers only.
  | { value: string; mode: "full"; fallback?: string };

export function IDText(props: Props) {
  const mode = (): Mode => props.mode ?? "short";
  const href = () => (props.mode === "link" ? props.href : undefined);

  return (
    <Show when={props.value} fallback={<span class="text-console-faint">{props.fallback ?? "—"}</span>}>
      <Show when={mode() === "full"} fallback={
        <Show
          when={href()}
          fallback={<span class={cx(TEXT, "text-console-muted")} title={props.value}>{formatID(props.value)}</span>}
        >
          {(target) => (
            <A href={target()} class={cx(TEXT, "text-console-accent hover:text-console-accent-hover")} title={props.value}>
              {formatID(props.value)}
            </A>
          )}
        </Show>
      }>
        <CopyText value={props.value} />
      </Show>
    </Show>
  );
}

function CopyText(props: { value: string }) {
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
        TEXT,
        "inline-flex max-w-full cursor-pointer items-baseline gap-1.5 break-all whitespace-normal border-0 bg-transparent p-0 text-left text-console-muted transition hover:text-console-text",
      )}
      title="Click to copy"
      aria-label={copied() ? `Copied ${props.value}` : `Copy ${props.value}`}
      onClick={() => void copy()}
    >
      <span>{props.value}</span>
      <span class="text-[10px] text-console-accent" aria-hidden="true">{copied() ? "Copied" : ""}</span>
    </button>
  );
}
