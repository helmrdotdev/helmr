import type { JSX } from "solid-js";
import { ui } from "./styles";

export function AuthScreen(props: { children: JSX.Element }) {
  return (
    <main class={"grid min-h-dvh place-items-center bg-transparent px-4 py-6"}>
      <section class={ui.panel}>
        <BrandHeader />
        {props.children}
      </section>
    </main>
  );
}

export function BrandHeader() {
  return (
    <div class={"mb-4 font-mono text-[12px] font-medium text-console-text"}>
      <span>Helmr</span>
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

export function AuthCopy(props: { children: JSX.Element }) {
  return <p class={ui.authCopy}>{props.children}</p>;
}

export function AuthActions(props: { children: JSX.Element }) {
  return <div class={ui.authActions}>{props.children}</div>;
}

export function AuthLoading(props: { children: JSX.Element }) {
  return (
    <AuthScreen>
      <p class={ui.muted}>{props.children}</p>
    </AuthScreen>
  );
}
