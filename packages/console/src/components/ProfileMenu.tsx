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
  const organizationSlug = () => me.data?.organizations.find((org) => org.id === me.data?.org_id)?.slug;

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

      <Popover open={open()} anchor={trigger()} onDismiss={() => setOpen(false)} role="menu" label="Profile" align="end" width={264}>
        <div class="flex flex-col gap-0.5 border-b border-console-border bg-console-bg-panel px-3 py-2">
          <span class={cx(ui.containerTitle, "text-console-text")}>{displayName()}</span>
          <Show when={me.data?.primary_email}>{email => <span class="max-w-full truncate text-chrome text-console-muted">{email()}</span>}</Show>
        </div>
        <Show when={me.data?.org_name}>
          {name => (
            <div class="flex flex-col gap-1.5 border-b border-console-border-soft px-3 py-2.5">
              <span class={ui.microLabel}>Organization</span>
              <span class="flex min-w-0 flex-col">
                <span class="truncate text-body font-medium text-console-text">{name()}</span>
                <span class="truncate font-mono text-micro text-console-muted">{[organizationSlug(), formatRole(me.data?.is_owner ? "owner" : "member")].filter(Boolean).join(" · ")}</span>
              </span>
              <A href="/organizations" class={ui.monoLink} role="menuitem" onClick={() => setOpen(false)}>Switch organization…</A>
            </div>
          )}
        </Show>
        <div class="p-1">
          <Show when={!me.data?.org_name}>
            <A href="/organizations" class={ui.scopeAction} role="menuitem" onClick={() => setOpen(false)}>Choose an organization</A>
          </Show>
          <A href="/settings/account" class={ui.scopeAction} role="menuitem" onClick={() => setOpen(false)}>Profile</A>
          <A href="/settings/security" class={ui.scopeAction} role="menuitem" onClick={() => setOpen(false)}>Sign-in and security</A>
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
