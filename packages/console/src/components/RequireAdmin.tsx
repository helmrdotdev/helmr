import { createQuery } from "@tanstack/solid-query";
import { Show, type JSX } from "solid-js";
import { getMe } from "../lib/auth";
import { AuthLoading } from "../ui/AuthScreen";
import { RequireAuth } from "./RequireAuth";

export function RequireAdmin(props: { children: JSX.Element }) {
  const me = createQuery(() => ({
    queryKey: ["me"],
    queryFn: getMe,
    retry: false,
    staleTime: 60_000,
  }));

  return (
    <RequireAuth allowOnboarding>
      <Show when={!me.isPending} fallback={<AuthLoading>Loading...</AuthLoading>}>
        <Show
          when={me.data?.admin}
          fallback={
            <main class="grid min-h-dvh place-items-center p-5">
              <p class="text-[13px] font-medium text-console-danger">Administrator access is required.</p>
            </main>
          }
        >
          {props.children}
        </Show>
      </Show>
    </RequireAuth>
  );
}
