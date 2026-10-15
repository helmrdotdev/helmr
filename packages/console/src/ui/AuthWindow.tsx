import { Show, type JSX } from "solid-js";
import { BrandMark } from "./BrandMark";
import { Spinner } from "./Skeleton";
import { cx, ui } from "./styles";

// The centered window for sign-in, invites, callbacks, Slack linking, device
// approval, access and onboarding screens. `step` shows onboarding progress in the
// container header ("Step 1 of 2").
export function AuthWindow(props: { children: JSX.Element; step?: string | undefined }) {
  return (
    <main class="grid min-h-dvh place-items-center bg-transparent px-4 py-6">
      <section class={cx(ui.container, "w-full max-w-98 text-console-text")}>
        <div class={ui.containerHeader}>
          <BrandMark />
          <Show when={props.step}><span class="font-mono text-chrome text-console-subtle">{props.step}</span></Show>
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

// Working: the server is finishing something the person just started (a
// sign-in callback, linking an account).
export function AuthWorking(props: { children: JSX.Element }) {
  return (
    <AuthWindow>
      <p class="flex items-center gap-2" role="status"><Spinner />{props.children}</p>
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

// Done: the result and the next step.
export function AuthSuccess(props: { title: string; children?: JSX.Element; action?: JSX.Element }) {
  return (
    <>
      <AuthTitle>{props.title}</AuthTitle>
      <Show when={props.children}><p role="status">{props.children}</p></Show>
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
