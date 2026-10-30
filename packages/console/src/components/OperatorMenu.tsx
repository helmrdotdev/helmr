import { A } from "@solidjs/router";
import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createSignal, onCleanup, Show } from "solid-js";
import { postJson, request } from "../lib/api";
import { errorMessage } from "../lib/error";
import { assertPasskey } from "../lib/passkey";
import { IDText } from "../ui/IDText";
import { Popover } from "../ui/Popover";
import { RelativeTime } from "../ui/RelativeTime";
import { cx, toneClass, ui } from "../ui/styles";

export type OperatorSession = { id: string; expires_at: string };

// Under an hour left, the trigger turns to the warning tone so renewal is due
// before the session ends mid-task.
const EXPIRY_WARNING_MS = 60 * 60_000;

// The admin area's identity control, where the console has the profile menu:
// which operator is signed in, when the session ends, renewal and sign-out.
export function OperatorMenu() {
  const cache = useQueryClient();
  const operator = createQuery(() => ({ queryKey: ["operator"], queryFn: () => request<OperatorSession>("/admin/api/v1/me", { redirectOnUnauthorized: false }), retry: false }));
  const [open, setOpen] = createSignal(false);
  const [trigger, setTrigger] = createSignal<HTMLButtonElement>();
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal("");
  const [renewed, setRenewed] = createSignal(false);
  // A tab left open must still turn the trigger to warning, so the check ticks.
  const [now, setNow] = createSignal(Date.now());
  const clock = setInterval(() => setNow(Date.now()), 60_000);
  onCleanup(() => clearInterval(clock));
  const expiring = () => {
    const expires = operator.data ? Date.parse(operator.data.expires_at) : Number.NaN;
    return !Number.isNaN(expires) && expires - now() < EXPIRY_WARNING_MS;
  };
  const renew = async () => {
    setBusy(true); setError(""); setRenewed(false);
    try {
      await assertPasskey("/admin/api/v1/auth/renew/options", "/admin/api/v1/auth/renew/verify");
      await cache.invalidateQueries({ queryKey: ["operator"] });
      setRenewed(true);
    } catch (cause) { setError(errorMessage(cause)); }
    finally { setBusy(false); }
  };
  const signOut = async () => {
    setBusy(true);
    try { await postJson("/admin/api/v1/auth/logout", {}); window.location.assign("/admin/login"); }
    catch (cause) { setError(errorMessage(cause)); setBusy(false); }
  };
  return (
    <div class="relative shrink-0">
      <button
        ref={setTrigger}
        type="button"
        class={cx(ui.secondaryButton, expiring() && toneClass.warning)}
        aria-haspopup="menu"
        aria-expanded={open()}
        aria-label="Open operator menu"
        onClick={() => setOpen(value => !value)}
      >
        Operator
      </button>
      <Popover open={open()} anchor={trigger()} onDismiss={() => setOpen(false)} role="menu" label="Operator" align="end" width={248}>
        <div class={ui.containerHeader}>
          <span class={cx(ui.containerTitle, "text-console-text")}>Operator</span>
          <span class="shrink-0 font-mono text-chrome"><IDText value={operator.data?.id ?? ""} /></span>
        </div>
        <div class="flex flex-col gap-0.5 border-b border-console-border px-3 py-2 text-body">
          <span class={expiring() ? "text-console-warning" : "text-console-muted"}>Session ends <RelativeTime value={operator.data?.expires_at} /></span>
          <Show when={renewed()}><span class="text-console-success" role="status">New 12-hour operator session started.</span></Show>
          <Show when={error()}><span class="text-console-danger" role="alert">{error()}</span></Show>
        </div>
        <div class="p-1">
          <button type="button" class={ui.scopeAction} role="menuitem" disabled={busy()} onClick={() => void renew()}>{busy() ? "Waiting for key…" : "Renew session"}</button>
          <A href="/admin/operator-keys" class={ui.scopeAction} role="menuitem" onClick={() => setOpen(false)}>Operator keys</A>
          <button type="button" class={ui.scopeAction} role="menuitem" disabled={busy()} onClick={() => void signOut()}>Sign out operator</button>
        </div>
      </Popover>
    </div>
  );
}
