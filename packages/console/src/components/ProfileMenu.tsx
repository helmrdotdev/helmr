import { A } from "@solidjs/router";
import { createMemo, createSignal, Show } from "solid-js";
import { logout } from "../lib/auth";
import { useMe } from "../lib/me";
import { Popover } from "../ui/Popover";
import { cx, ui } from "../ui/styles";

export function formatRole(role: string | null | undefined): string {
  if (!role) return "Member";
  return role
    .split(/[_\s-]+/)
    .filter(Boolean)
    .map((part) => part.charAt(0).toUpperCase() + part.slice(1).toLowerCase())
    .join(" ");
}

function initialsFor(value: string): string {
  const parts = value.trim().split(/\s+/).filter(Boolean);
  if (parts.length === 0) return "?";
  if (parts.length === 1) return parts[0]!.slice(0, 2).toUpperCase();
  return `${parts[0]!.charAt(0)}${parts[parts.length - 1]!.charAt(0)}`.toUpperCase();
}

export function ProfileMenu() {
  const me = useMe();
  const [open, setOpen] = createSignal(false);
  const [imageFailed, setImageFailed] = createSignal(false);
  const [trigger, setTrigger] = createSignal<HTMLButtonElement>();

  const displayName = createMemo(() => me.data?.display_name?.trim() || me.data?.user_id || "Account");
  const imageURL = createMemo(() => me.data?.profile_image_url?.trim() || null);
  const showImage = createMemo(() => !!imageURL() && !imageFailed());

  return (
    <div class="relative shrink-0">
      <button
        ref={setTrigger}
        type="button"
        class="grid size-7 cursor-pointer place-items-center overflow-hidden rounded-xs border border-console-border gradient-button p-0 font-mono text-[10px] font-medium leading-none text-console-muted transition duration-100 hover:border-console-border-strong hover:gradient-button-hover hover:text-console-text data-[open=true]:border-console-border-strong data-[open=true]:bg-console-bg-panel data-[open=true]:text-console-text"
        data-open={open() ? "true" : "false"}
        aria-haspopup="menu"
        aria-expanded={open()}
        aria-label="Open profile menu"
        onClick={() => setOpen((value) => !value)}
      >
        <Show when={showImage()} fallback={<span class="select-none leading-none">{initialsFor(displayName())}</span>}>
          <img
            src={imageURL()!}
            alt=""
            class="size-full object-cover grayscale-[0.15]"
            referrerpolicy="no-referrer"
            onError={() => setImageFailed(true)}
          />
        </Show>
      </button>

      <Popover open={open()} anchor={trigger()} onDismiss={() => setOpen(false)} role="menu" label="Profile" align="end" width={224}>
        <div class={ui.containerHeader}>
          <span class={cx(ui.containerTitle, "text-console-text")}>{displayName()}</span>
          <span class={cx(ui.microLabel, "shrink-0")}>{formatRole(me.data?.is_owner ? "owner" : "member")}</span>
        </div>
        <div class="p-1">
          <A href="/settings/account" class={ui.scopeAction} role="menuitem" onClick={() => setOpen(false)}>
            Account settings
          </A>
          <Show when={me.data?.admin}>
            <A href="/admin" class={ui.scopeAction} role="menuitem" onClick={() => setOpen(false)}>
              Admin
            </A>
          </Show>
          <button
            type="button"
            class={ui.scopeAction}
            role="menuitem"
            onClick={() => {
              setOpen(false);
              void logout();
            }}
          >
            Sign out
          </button>
        </div>
      </Popover>
    </div>
  );
}
