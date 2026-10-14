import { createInfiniteQuery } from "@tanstack/solid-query";
import { createEffect, createMemo, createSignal, For, onCleanup, Show } from "solid-js";
import type { Failure } from "../../lib/error";
import { slackFailure, slackNotConfigured } from "./failure";
import { SlackFailureNotice, SlackNotSetUp } from "./SlackFailure";
import { listSlackLinkWorkspaces, listSlackUserLinks, startSlackUserLink, unlinkSlackUser, type SlackUserLink } from "../../lib/slack";
import { LoadMore } from "../../ui/DataTable";
import { ConfirmDialog } from "../../ui/Dialog";
import { Field } from "../../ui/Field";
import { IDText } from "../../ui/IDText";
import { List, ListItem } from "../../ui/List";
import { Panel } from "../../ui/Panel";
import { QueryState } from "../../ui/QueryState";
import { Select } from "../../ui/Select";
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
  // Every workspace belongs in the picker, which filters long lists itself.
  createEffect(() => {
    if (workspaces.hasNextPage && !workspaces.isFetchingNextPage && !workspaces.isError) void workspaces.fetchNextPage();
  });
  const allLinks = createMemo(() => links.data?.pages.flatMap((p) => p.links) ?? []);
  const allWorkspaces = createMemo(() => workspaces.data?.pages.flatMap((p) => p.workspaces) ?? []);
  const workspaceName = (teamID: string) => allWorkspaces().find((w) => w.team_id === teamID)?.workspace_name || undefined;
  const [workspace, setWorkspace] = createSignal("");
  const [unlinking, setUnlinking] = createSignal<SlackUserLink | null>(null);
  const [busy, setBusy] = createSignal(false), [error, setError] = createSignal<Failure | null>(null);
  let mounted = true; onCleanup(() => { mounted = false; });
  const start = async () => {
    setBusy(true); setError(null);
    try { const result = await startSlackUserLink(workspace()); if (mounted) window.location.assign(result.redirect_url); }
    catch (e) { if (mounted) { setError(slackFailure(e, "Slack identity linking could not start.")); setBusy(false); } }
  };
  return <Panel title="Your Slack identity">
    <p class={ui.muted}>Link the Slack account you use to request work and answer questions. Your existing Helmr permissions still apply.</p>
    <Show when={!slackNotConfigured(links.error) && !slackNotConfigured(workspaces.error)} fallback={<SlackNotSetUp failure={slackFailure(links.error ?? workspaces.error, "")} consequence="Slack identities cannot be linked." />}>
    <Show when={error()}>{(failure) => <SlackFailureNotice failure={failure()} />}</Show>
    <QueryState
      query={links}
      pending="Loading linked identities..."
      error={(e) => slackFailure(e, "Your Slack identity settings could not be loaded.").message}
      empty={{ when: (data) => data.pages.every((page) => page.links.length === 0), title: "No Slack identities linked." }}
    >
      {() => (
        <List
          label="Linked Slack identities"
          footer={<LoadMore count={allLinks().length} noun={{ one: "linked identity", other: "linked identities" }} hasMore={links.hasNextPage} loading={links.isFetchingNextPage} failed={links.isFetchNextPageError} onLoadMore={() => void links.fetchNextPage()} />}
        >
          <For each={allLinks()}>{(link) => <ListItem line class="flex-wrap justify-between">
            <span class="inline-flex flex-wrap items-baseline gap-1.5">
              <IDText value={link.slack_user_id} />
              <span class={ui.muted}>in {workspaceName(link.team_id) || <IDText value={link.team_id} />}</span>
            </span>
            <button type="button" class={ui.secondaryButton} disabled={busy()} aria-label={`Unlink ${link.slack_user_id}`} onClick={() => { setError(null); setUnlinking(link); }}>Unlink</button>
          </ListItem>}</For>
        </List>
      )}
    </QueryState>
    <Show when={workspaces.isError}><SlackFailureNotice failure={slackFailure(workspaces.error, "Slack workspaces could not be loaded.")} /></Show>
    <Show when={workspaces.data?.pages[0]?.configured}>
      <Show when={allWorkspaces().length > 0} fallback={<p class={ui.muted}>An organization admin must connect an agent to Slack before you can link your account here.</p>}>
        <div class="flex flex-wrap items-end gap-3">
          <Field label="Workspace for your identity" flush class="min-w-52">
            <Select value={workspace()} disabled={busy()} placeholder="Select workspace" options={allWorkspaces().map((w) => ({ value: w.installation_id, label: w.workspace_name || w.team_id }))} onChange={setWorkspace} minWidth="100%" />
          </Field>
          <button type="button" class={ui.secondaryButton} disabled={busy() || !workspace()} onClick={() => void start()}>Verify Slack identity</button>
        </div>
      </Show>
    </Show>
    </Show>
    <Show when={unlinking()}>{(link) => (
      <ConfirmDialog
        title="Unlink Slack identity"
        confirmLabel="Unlink identity"
        busyLabel="Unlinking..."
        cancelLabel="Keep identity linked"
        tone="danger"
        target={<IDText value={link().slack_user_id} />}
        errorMessage={(e) => slackFailure(e, "Slack identity could not be unlinked.").message}
        onClose={() => setUnlinking(null)}
        onConfirm={async () => {
          await unlinkSlackUser(link());
          if (mounted) await links.refetch();
        }}
      >
        Future Slack requests and answers from this account require linking again. Work already accepted keeps its original user identity and continues.
      </ConfirmDialog>
    )}</Show>
  </Panel>;
}
