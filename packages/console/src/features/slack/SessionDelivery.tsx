import { createInfiniteQuery } from "@tanstack/solid-query";
import { createMemo, createSignal, createUniqueId, For, Show } from "solid-js";
import type { Failure } from "../../lib/error";
import { slackFailure } from "./failure";

const DELIVERY_MESSAGES = { conflict: "This message's delivery changed since the page loaded; refresh before trying again." };
import { SlackFailureNotice } from "./SlackFailure";
import type { SessionAddress } from "../../lib/sessions";
import { listSlackDelivery, recoverSlackDelivery, type SlackDeliveryPost } from "../../lib/slack";
import { CodeBlock } from "../../ui/CodeBlock";
import { LoadMore } from "../../ui/DataTable";
import { ConfirmDialog } from "../../ui/Dialog";
import { List, ListItem } from "../../ui/List";
import { Panel } from "../../ui/Panel";
import { QueryState } from "../../ui/QueryState";
import { LinesSkeleton } from "../../ui/Skeleton";
import { RelativeTime } from "../../ui/RelativeTime";
import { cx, ui } from "../../ui/styles";

function checkReason(post: SlackDeliveryPost): string {
  if (post.error === "history_no_match") return "No matching Slack message was found. This does not mean it was never delivered.";
  if (post.error === "history_page_limit") return "The recent-message check reached its limit without confirming delivery.";
  if (post.error?.startsWith("history_") || post.error === "invalid_history_response") return "Slack message history could not confirm this delivery.";
  return "Delivery could not be confirmed. Some content may already be in Slack.";
}
function title(post: SlackDeliveryPost): string {
  if (post.disposed_at) return post.confirmed_revision > 0 ? "Delivery stopped; some content confirmed" : "Delivery stopped; remote outcome unconfirmed";
  return post.status === "uncertain" ? "Delivery unconfirmed" : post.status === "suppressed" ? "Delivery suppressed" : "Delivery failed";
}

function roleLabel(post: SlackDeliveryPost): string {
  return post.role === "intermediate" ? "Progress" : post.role === "request_feedback" ? "Acknowledgment" : post.role === "question" ? "Question" : post.role === "response" ? "Response" : "Task status";
}

// One delivery issue: a 36px row with its actions; opened, the message text and
// what is known about its delivery, as plain text under the row.
function DeliveryRow(props: { post: SlackDeliveryPost; open: boolean; onToggle: () => void; canManage: boolean; checking: boolean; onCheck: () => void; onStop: () => void }) {
  const open = () => props.open;
  const post = () => props.post;
  const bodyID = createUniqueId();
  const titleID = createUniqueId();
  return (
    <ListItem class="p-0!">
      <div class="flex min-h-9 flex-wrap items-center gap-x-3 gap-y-1 px-3 py-1">
        <button type="button" class="flex min-w-0 flex-1 cursor-pointer items-center gap-2 text-left" aria-expanded={open()} aria-controls={bodyID} onClick={() => props.onToggle()}>
          <span aria-hidden="true" class={cx("text-chrome text-console-subtle transition-transform", open() && "rotate-90")}>▸</span>
          <span id={titleID} class="font-medium text-console-text">{title(post())}</span>
          <span class="truncate text-console-muted">· {roleLabel(post())} · message {post().sequence} · <RelativeTime value={post().created_at} /></span>
        </button>
        <Show when={post().attempt_id && !post().check_paused_at}><span class="font-mono text-chrome text-console-subtle">Check scheduled</span></Show>
        <Show when={props.canManage && post().attempt_id}>
          <Show when={post().check_paused_at}><button type="button" class={ui.compactButton} aria-describedby={titleID} disabled={props.checking} onClick={() => props.onCheck()}>Check Slack again</button></Show>
          <Show when={post().status === "uncertain" && !post().disposed_at}><button type="button" class={ui.compactButton} aria-describedby={titleID} disabled={props.checking} onClick={() => props.onStop()}>Stop delivery</button></Show>
        </Show>
      </div>
      <div id={bodyID} hidden={!open()} class="flex flex-col gap-1.5 px-3 pb-3 pl-8">
        <Show when={!post().payload_expired_at} fallback={<p class={ui.muted}>Message content expired under the session’s retention policy.</p>}>
          <p class="m-0 whitespace-pre-wrap break-words text-console-text">{post().text}</p>
        </Show>
        <Show when={post().status === "uncertain"}><p class={ui.muted}>{checkReason(post())}</p></Show>
        <Show when={!post().root_known}><p class={ui.muted}>The thread’s first message is unconfirmed. Later messages remain blocked until Slack confirms that original message.</p></Show>
        <Show when={post().disposed_at}>
          <p class={ui.muted}>Remaining parts of this message will not be sent. Agent work continues.</p>
          <Show when={post().stream_state === "uncertain" || post().stream_state === "open"}><p class={ui.muted}>Slack stream termination remains unconfirmed. Evidence is retained until it can be resolved.</p></Show>
        </Show>
        <Show when={post().message_ts && post().confirmed_revision > 0}><p class={ui.muted}>{post().confirmed_revision === post().desired_revision ? "Slack confirmed this message." : "Slack confirmed an earlier version of this message."}</p></Show>
      </div>
    </ListItem>
  );
}

export function SessionDelivery(props: { address: SessionAddress; live: boolean; canManage: boolean }) {
  const posts = createInfiniteQuery(() => ({
    queryKey: ["slack-delivery", props.address.projectID, props.address.environmentID, props.address.sessionID],
    queryFn: ({ pageParam }) => listSlackDelivery(props.address, pageParam),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (page) => page.next_cursor,
    refetchInterval: props.live ? 5_000 : false,
    retry: false,
  }));
  const rows = createMemo(() => posts.data?.pages.flatMap((page) => page.posts) ?? []);
  const [selected, setSelected] = createSignal<SlackDeliveryPost>();
  const [checking, setChecking] = createSignal(false);
  // Opened rows by post ID, so a refetch that replaces a post keeps its row
  // open. The first issue, and any that blocks later messages, start open.
  const [toggled, setToggled] = createSignal<ReadonlySet<string>>(new Set());
  const startsOpen = (post: SlackDeliveryPost, index: number) => index === 0 || !post.root_known;
  const isOpen = (post: SlackDeliveryPost, index: number) => startsOpen(post, index) !== toggled().has(post.id);
  const toggle = (post: SlackDeliveryPost) => {
    const next = new Set(toggled());
    if (next.has(post.id)) next.delete(post.id); else next.add(post.id);
    setToggled(next);
  };
  const [error, setError] = createSignal<Failure>();
  const check = async (post: SlackDeliveryPost) => {
    setChecking(true); setError(undefined);
    try { await recoverSlackDelivery(props.address, post, "check"); await posts.refetch(); }
    catch (cause) { setError(slackFailure(cause, "Delivery could not be checked.", DELIVERY_MESSAGES)); }
    finally { setChecking(false); }
  };
  return <Panel title="Slack delivery">
    <Show when={error()}>{(failure) => <SlackFailureNotice failure={failure()} />}</Show>
    <Show when={!props.canManage && rows().some((post) => post.attempt_id)}>
      <p class={cx(ui.muted, "m-0")}>An organization owner or admin can check again or stop delivery.</p>
    </Show>
    <QueryState
      query={posts}
      pending={<LinesSkeleton label="Loading Slack delivery" />}
      error={(e) => slackFailure(e, "Slack delivery could not be loaded.").message}
      empty={{ when: (data) => data.pages.every((page) => page.posts.length === 0), title: "No Slack delivery issues reported." }}
    >
      {() => (
        <List
          label="Slack delivery issues"
          footer={<LoadMore count={rows().length} noun={{ one: "delivery issue", other: "delivery issues" }} hasMore={posts.hasNextPage} loading={posts.isFetchingNextPage} failed={posts.isFetchNextPageError} onLoadMore={() => void posts.fetchNextPage()} />}
        >
          <For each={rows()}>{(post, index) => <DeliveryRow post={post} open={isOpen(post, index())} onToggle={() => toggle(post)} canManage={props.canManage} checking={checking()} onCheck={() => void check(post)} onStop={() => setSelected(post)} />}</For>
        </List>
      )}
    </QueryState>
    <Show when={selected()} keyed>{(post) => <ConfirmDialog cancelLabel="Keep" errorMessage={(e) => slackFailure(e, "Delivery could not be stopped.", DELIVERY_MESSAGES).message} title="Stop delivery of this message?" confirmLabel="Stop delivery" target={`Message ${post.sequence}`} onClose={() => setSelected(undefined)} onConfirm={async () => { await recoverSlackDelivery(props.address, post, "abandon"); await posts.refetch(); }}>
      <div class={ui.blockStack}>
        <p class="m-0">Stop sending the rest of this message, including its continuation parts. Some content may already be in Slack. This does not stop the agent.</p>
        <CodeBlock code={post.text} label="Message content" />
        <Show when={!post.root_known}><p class="m-0">The original thread must still be confirmed before later messages can be sent. This action will not create a replacement thread.</p></Show>
        <Show when={post.stream_state === "uncertain" || post.stream_state === "open"}><p class="m-0">Evidence remains while Slack stream termination is unconfirmed; this action does not guarantee content expiry.</p></Show>
      </div>
    </ConfirmDialog>}</Show>
  </Panel>;
}
