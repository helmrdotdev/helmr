import { Show, type JSX } from "solid-js";
import { BrandMark } from "./BrandMark";
import { Progress } from "./Progress";
import { cx, ui } from "./styles";

// The centered window for sign-in, invites, callbacks, Slack linking, device
// approval, access and onboarding screens. `step` shows onboarding progress in the
// title strip ("Step 1 of 2").
export function AuthWindow(props: { children: JSX.Element; step?: string | undefined }) {
  return (
    <main class="grid min-h-dvh place-items-center bg-transparent px-4 py-6">
      <section class={cx(ui.window, "w-full max-w-98 text-console-text")}>
        <div class={ui.titleStrip}>
          <BrandMark />
          <Show when={props.step}><span class="text-console-subtle">{props.step}</span></Show>
        </div>
        <div class="px-4 pb-4 pt-4 [:where(&_a)]:mt-3 [:where(&_a)]:inline-block [:where(&_a)]:cursor-pointer [:where(&_a)]:text-body [:where(&_a)]:text-console-accent [:where(&_a:hover)]:text-console-accent-hover [&_button]:w-full [&_p]:text-body [&_p]:leading-normal [&_p]:text-console-muted">
          {props.children}
        </div>
      </section>
    </main>
  );
}

export function AuthDivider(props: { children: JSX.Element }) {
  return (
    <div class={ui.authDivider}>
      <span />
      <span>{props.children}</span>
      <span />
    </div>
  );
}

export function AuthTitle(props: { children: JSX.Element }) {
  return <h1 class={ui.authTitle}>{props.children}</h1>;
}

export function AuthCopy(props: { children: JSX.Element }) {
  return <p class={ui.authCopy}>{props.children}</p>;
}

export function AuthActions(props: { children: JSX.Element }) {
  return <div class={ui.authActions}>{props.children}</div>;
}

// Working: a request or redirect is in progress.
export function AuthWorking(props: { children: JSX.Element }) {
  return (
    <AuthWindow>
      <div class={ui.windowStack}>
        <Progress label="Working" />
        <p class={ui.muted}>{props.children}</p>
      </div>
    </AuthWindow>
  );
}

// An actionable failure: a human message, the next step, and the technical
// detail folded away for support.
export function AuthError(props: { title?: string; message: string; details?: string | undefined; action?: JSX.Element }) {
  return (
    <>
      <Show when={props.title}><AuthTitle>{props.title}</AuthTitle></Show>
      <p class="font-medium text-console-danger!" role="alert">{props.message}</p>
      <Show when={props.details}>
        <details class="mt-2 text-chrome text-console-muted">
          <summary class="cursor-pointer font-mono">Technical details</summary>
          <pre class="mt-1.5 overflow-x-auto whitespace-pre-wrap break-words border border-console-border bg-console-bg-panel p-2 font-mono text-badge">{props.details}</pre>
        </details>
      </Show>
      <Show when={props.action}>
        <AuthActions>{props.action}</AuthActions>
      </Show>
    </>
  );
}

// Done: the result and, while the next page loads, a redirect bar.
export function AuthSuccess(props: { title: string; children?: JSX.Element; redirecting?: boolean; action?: JSX.Element }) {
  return (
    <>
      <AuthTitle>{props.title}</AuthTitle>
      <Show when={props.children}><p role="status">{props.children}</p></Show>
      <Show when={props.redirecting}><Progress label="Redirecting" class="mt-3" /></Show>
      <Show when={props.action}><AuthActions>{props.action}</AuthActions></Show>
    </>
  );
}

// The link or code this screen needs is missing or has expired: say which, and
// how to get a new one.
export function AuthMissing(props: { title: string; children: JSX.Element; action?: JSX.Element }) {
  return (
    <>
      <AuthTitle>{props.title}</AuthTitle>
      <p>{props.children}</p>
      <Show when={props.action}><AuthActions>{props.action}</AuthActions></Show>
    </>
  );
}
