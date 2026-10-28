import { A } from "@solidjs/router";
import { Show, type JSX } from "solid-js";
import { useMe } from "../lib/me";
import { AuthMissing, AuthWindow } from "../ui/AuthWindow";
import { ui } from "../ui/styles";
import { RequireAuth } from "./RequireAuth";

export function RequireAdmin(props: { children: JSX.Element }) {
  const me = useMe();

  return (
    <RequireAuth requirement="session">
      <Show when={!me.isPending}>
        <Show
          when={me.data?.admin}
          fallback={
            <AuthWindow>
              <AuthMissing title="Administrator access required" action={<A href="/" class={ui.secondaryButton}>Back to the console</A>}>
                The admin area is for Helmr administrators. Sign in with an administrator account to open it.
              </AuthMissing>
            </AuthWindow>
          }
        >
          {props.children}
        </Show>
      </Show>
    </RequireAuth>
  );
}
