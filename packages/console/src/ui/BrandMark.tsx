import { A } from "@solidjs/router";
import { Show } from "solid-js";

const MARK = "font-mono text-control font-medium text-console-text";

// The product name as it appears in bars and auth windows, optionally naming the
// area ("Admin"). With `href` it links home.
export function BrandMark(props: { area?: string; href?: string }) {
  const label = () => (props.area ? `Helmr ${props.area}` : "Helmr");
  return (
    <Show when={props.href} fallback={<span class={MARK}>{label()}</span>}>
      {(href) => <A href={href()} class={`${MARK} shrink-0 hover:text-console-accent`}>{label()}</A>}
    </Show>
  );
}
