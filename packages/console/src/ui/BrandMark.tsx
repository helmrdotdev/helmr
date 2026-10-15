import { A } from "@solidjs/router";
import { Show } from "solid-js";

// The Helmr mark and wordmark, set as on the website: the name in Instrument Sans
// at 19px, the mark 1.5em wide and 0.35em from it. `area` names a part of the
// product after it ("Admin"); with `href` it links home.
export function BrandMark(props: { area?: string; href?: string }) {
  const content = () => (
    <>
      <span class="inline-flex items-center gap-[0.35em] text-[19px]">
        <img src="/brand/helmr-mark.svg" alt="" width="29" height="17" class="h-auto w-[1.5em] shrink-0" />
        <span class="font-console-brand font-[550] leading-6 tracking-[-0.025em] text-console-text">Helmr</span>
      </span>
      <Show when={props.area}><span class="font-mono text-control font-medium text-console-muted">{props.area}</span></Show>
    </>
  );
  return (
    <Show when={props.href} fallback={<span class="inline-flex shrink-0 items-center gap-2">{content()}</span>}>
      {(href) => <A href={href()} aria-label={props.area ? `Helmr ${props.area} home` : "Helmr home"} class="inline-flex shrink-0 items-center gap-2 hover:opacity-80">{content()}</A>}
    </Show>
  );
}
