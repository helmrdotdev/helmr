import { createInfiniteQuery } from "@tanstack/solid-query";
import { createMemo, createSignal, For, onCleanup, Show } from "solid-js";
import { listSlackLinkWorkspaces, listSlackUserLinks, startSlackUserLink, unlinkSlackUser, type SlackUserLink } from "../../lib/slack";
import { Modal } from "../../ui/Modal";
import { ui } from "../../ui/styles";

export function IdentitySettings(props: { userID: string; organizationID: string }) {
  const links = createInfiniteQuery(() => ({
    queryKey: ["slack-user-links", props.userID], queryFn: ({ pageParam }) => listSlackUserLinks(pageParam),
    initialPageParam: undefined as string | undefined, getNextPageParam: (page) => page.next_cursor,
  }));
  const workspaces = createInfiniteQuery(() => ({
    queryKey: ["slack-link-workspaces", props.organizationID], queryFn: ({ pageParam }) => listSlackLinkWorkspaces(pageParam),
    initialPageParam: undefined as string | undefined, getNextPageParam: (page) => page.next_cursor,
  }));
  const allLinks = createMemo(() => links.data?.pages.flatMap((p) => p.links) ?? []);
  const allWorkspaces = createMemo(() => workspaces.data?.pages.flatMap((p) => p.workspaces) ?? []);
  const [workspace, setWorkspace] = createSignal("");
  const [unlinking, setUnlinking] = createSignal<SlackUserLink | null>(null);
  const [busy, setBusy] = createSignal(false), [error, setError] = createSignal<string | null>(null);
  let mounted = true; onCleanup(() => { mounted = false; });
  const start = async () => {
    setBusy(true); setError(null);
    try { const result = await startSlackUserLink(workspace()); if (mounted) window.location.assign(result.redirect_url); }
    catch (e) { if (mounted) { setError(e instanceof Error ? e.message : "Slack identity linking could not start."); setBusy(false); } }
  };
  const unlink = async () => {
    const current = unlinking(); if (!current) return;
    setBusy(true); setError(null);
    try { await unlinkSlackUser(current); if (mounted) { setUnlinking(null); await links.refetch(); } }
    catch (e) { if (mounted) setError(e instanceof Error ? e.message : "Slack identity could not be unlinked."); }
    finally { if (mounted) setBusy(false); }
  };
  return <section class="flex flex-col gap-3 border border-console-border bg-console-surface p-4">
    <div><h2 class={ui.h2}>Your Slack identity</h2><p class={ui.muted}>Link the Slack account you use to request work and answer questions. Your existing Helmr permissions still apply.</p></div>
    <Show when={links.error || workspaces.error}><p class={ui.error}>Your Slack identity settings could not be loaded.</p></Show>
    <Show when={error()}><p class={ui.error} role="alert">{error()}</p></Show>
    <For each={allLinks()}>{(link) => <div class="flex flex-wrap items-center justify-between gap-3">
      <p>{link.slack_user_id} <span class={ui.muted}>in {allWorkspaces().find((w) => w.team_id === link.team_id)?.workspace_name || link.team_id}</span></p>
      <button class={ui.secondaryButton} disabled={busy()} onClick={() => { setError(null); setUnlinking(link); }}>Unlink {link.slack_user_id}</button>
    </div>}</For>
    <Show when={links.hasNextPage}><button class={ui.secondaryButton} disabled={links.isFetchingNextPage} onClick={() => void links.fetchNextPage()}>Load more linked identities</button></Show>
    <Show when={!links.isPending && !links.error && allLinks().length === 0}><p class={ui.muted}>No Slack identities linked.</p></Show>
    <Show when={workspaces.data?.pages[0]?.configured}>
      <div class="flex flex-wrap items-end gap-3"><label class="flex min-w-52 flex-col gap-1">Workspace for your identity<select class={ui.input} value={workspace()} disabled={busy()} onChange={(e) => setWorkspace(e.currentTarget.value)}><option value="">Select workspace</option><For each={allWorkspaces()}>{(w) => <option value={w.installation_id}>{w.workspace_name || w.team_id}</option>}</For></select></label><button class={ui.secondaryButton} disabled={busy() || !workspace()} onClick={() => void start()}>Verify Slack identity</button></div>
      <Show when={workspaces.hasNextPage}><button class={ui.secondaryButton} disabled={workspaces.isFetchingNextPage} onClick={() => void workspaces.fetchNextPage()}>Load more available workspaces</button></Show>
      <Show when={!workspaces.isPending && !workspaces.error && allWorkspaces().length === 0}><p class={ui.muted}>An organization admin must connect an Agent to Slack before you can link your account here.</p></Show>
    </Show>
    <Show when={unlinking()}>{(link) => <Modal title={`Unlink ${link().slack_user_id}?`} onClose={() => setUnlinking(null)} closeDisabled={busy()}>
      <p>Future Slack requests and answers from this account require linking again. Work already accepted keeps its original user identity and continues.</p>
      <Show when={error()}><p class={ui.error} role="alert">{error()}</p></Show>
      <div class={ui.modalActions}><button class={ui.secondaryButton} disabled={busy()} onClick={() => setUnlinking(null)}>Keep identity linked</button><button class={ui.dangerOutlineButton} disabled={busy()} onClick={() => void unlink()}>Unlink identity</button></div>
    </Modal>}</Show>
  </section>;
}
