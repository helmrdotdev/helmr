import type { JSX } from "solid-js";

// A short fixed fact after a name: owner, you, working, default. One shape
// everywhere, in words rather than a symbol, so it reads without explanation.
export function Tag(props: { children: JSX.Element; title?: string | undefined }) {
  return (
    <span class="inline-flex h-4.5 shrink-0 items-center rounded-xs border border-console-border bg-console-bg-panel px-1.5 font-mono text-micro font-medium leading-none text-console-muted" title={props.title}>
      {props.children}
    </span>
  );
}
