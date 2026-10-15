import { A } from "@solidjs/router";
import { BrandMark } from "./BrandMark";
import { Show, type JSX } from "solid-js";
import { Spinner } from "./Skeleton";
import { cx, ui } from "./styles";

// The page for sign-in, invites, callbacks, Slack linking, device approval,
// access and onboarding screens: the product mark in the top-left corner, no
// window or card, and one left-aligned column. `step` labels onboarding progress
// above the title ("Step 1 of 2"); `account` sits in the top-right corner for a
// signed-in account (see AuthAccount). In the column, buttons and button links
// run full width; one primary action, secondary actions as AuthLink under it.
export function AuthWindow(props: { children: JSX.Element; step?: string | undefined; account?: JSX.Element }) {
  return (
    <main class="flex min-h-dvh flex-col px-4 text-console-text">
      <header class="flex h-12 shrink-0 items-center justify-between gap-3">
        <BrandMark href="/" />
        <Show when={props.account}>{props.account}</Show>
      </header>
      <section class="mx-auto flex w-full max-w-90 flex-1 flex-col justify-center pb-[12vh] pt-6 [&_.auth-full]:w-full [&_button]:w-full [&_p]:text-body [&_p]:leading-normal [&_p]:text-console-muted">
        <Show when={props.step}><span class={cx(ui.microLabel, "mb-2")}>{props.step}</span></Show>
        {props.children}
      </section>
    </main>
  );
}

// The signed-in account and Sign out, for the top-right corner of onboarding.
export function AuthAccount(props: { name: string | null | undefined; signOutLabel: string; signingOut: boolean; onSignOut: () => void }) {
  return (
    <div class="flex min-w-0 items-center gap-2 font-mono text-chrome text-console-muted">
      <Show when={props.name}><span class="truncate">{props.name}</span><span aria-hidden="true">·</span></Show>
      <button type="button" class="cursor-pointer text-console-accent hover:text-console-accent-hover disabled:cursor-not-allowed disabled:opacity-60" disabled={props.signingOut} onClick={() => props.onSignOut()}>
        {props.signOutLabel}
      </button>
    </div>
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

// The line under the title: what this screen is for.
export function AuthCopy(props: { children: JSX.Element }) {
  return <p class={ui.authCopy}>{props.children}</p>;
}

export function AuthActions(props: { children: JSX.Element }) {
  return <div class={ui.authActions}>{props.children}</div>;
}

// A secondary action under the primary one, as centered link text.
export function AuthLink(props: { children: JSX.Element; onClick?: () => void; href?: string; disabled?: boolean }) {
  return (
    <Show
      when={props.href}
      fallback={<button type="button" class={ui.authLink} disabled={props.disabled} onClick={() => props.onClick?.()}>{props.children}</button>}
    >
      {(href) => <A href={href()} class={ui.authLink}>{props.children}</A>}
    </Show>
  );
}

// Facts the person checks before acting (the account, the organization), as
// label/value rows between two hairlines.
export function AuthFacts(props: { label: string; children: JSX.Element }) {
  return <dl class={ui.authFacts} aria-label={props.label}>{props.children}</dl>;
}

export function AuthFact(props: { label: string; children: JSX.Element }) {
  return (
    <>
      <dt class="text-console-muted">{props.label}</dt>
      <dd class="m-0 min-w-0 [overflow-wrap:anywhere]">{props.children}</dd>
    </>
  );
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

// An actionable failure: a human message, the technical detail folded away for
// support, then the next step. Inside a form it sits directly above the
// primary button. Input mistakes pass no details.
export function AuthError(props: { title?: string; message: string; details?: string | undefined; action?: JSX.Element }) {
  return (
    <>
      <Show when={props.title}><AuthTitle>{props.title}</AuthTitle></Show>
      <p class="mb-2 font-medium text-console-danger!" role="alert">{props.message}</p>
      <Show when={props.details}>
        <details class="mb-3 text-chrome text-console-muted">
          <summary class="cursor-pointer font-mono">Technical details</summary>
          <p class="m-0 mt-1 whitespace-pre-wrap break-words font-mono text-badge! text-console-muted">{props.details}</p>
        </details>
      </Show>
      <Show when={props.action}>
        <AuthActions>{props.action}</AuthActions>
      </Show>
    </>
  );
}

// Done: one title, one sentence naming the next step, and its action.
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
