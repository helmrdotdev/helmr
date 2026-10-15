import { A } from "@solidjs/router";
import { Show } from "solid-js";

// The Helmr mark and wordmark, set as on the website: the mark 26px wide, 8px
// from the name in Instrument Sans. `area` names a part of the product after it
// ("Admin"); with `href` it links home.
export function BrandMark(props: { area?: string; href?: string }) {
  const content = () => (
    <>
      <img src="/brand/helmr-mark.svg" alt="" width="26" height="15" class="h-[15px] w-[26px] shrink-0" />
      <span class="font-console-brand text-[16px] font-[550] leading-6 text-console-text">Helmr</span>
      <Show when={props.area}><span class="font-mono text-control font-medium text-console-muted">{props.area}</span></Show>
    </>
  );
  return (
    <Show when={props.href} fallback={<span class="inline-flex items-center gap-2">{content()}</span>}>
      {(href) => <A href={href()} aria-label={props.area ? `Helmr ${props.area} home` : "Helmr home"} class="inline-flex shrink-0 items-center gap-2 hover:opacity-80">{content()}</A>}
    </Show>
  );
}
