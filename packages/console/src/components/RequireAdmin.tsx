import { A } from "@solidjs/router";
import { Show, type JSX } from "solid-js";
import { useMe } from "../lib/me";
import { AuthError, AuthWindow } from "../ui/AuthWindow";
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
              <AuthError
                title="Administrator access required"
                message="Your account cannot open the admin area. Sign in with an administrator account."
                action={<A href="/" class={ui.secondaryButton}>Back to the console</A>}
              />
            </AuthWindow>
          }
        >
          {props.children}
        </Show>
      </Show>
    </RequireAuth>
  );
}
