import { createQuery } from "@tanstack/solid-query";
import { Show } from "solid-js";
import { getMe } from "../lib/auth";
import { IdentitySettings } from "../features/slack/IdentitySettings";
import { ui } from "../ui/styles";

export function SlackSettings() {
  const me = createQuery(() => ({ queryKey: ["me"], queryFn: getMe, retry: false }));
  return <div class="flex flex-col gap-5">
    <div><h1 class={ui.h1}>Your Slack account</h1><p class={ui.muted}>Link your Slack account to your Helmr account. This personal link works across all Agent apps in that Slack workspace, using your current Helmr permissions.</p></div>
    <p class={ui.muted}>To connect an Agent, open its Slack connection from the deployment’s Agents list.</p>
    <Show when={me.error}><p class={ui.error}>Your account could not be loaded.</p></Show>
    <Show when={me.data?.org_id} keyed>{(org) => <IdentitySettings userID={me.data!.user_id} organizationID={org} />}</Show>
  </div>;
}
