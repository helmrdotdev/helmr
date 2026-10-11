import { createInfiniteQuery } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import type { SessionAddress } from "../../lib/sessions";
import { listSlackDelivery, recoverSlackDelivery, type SlackDeliveryPost } from "../../lib/slack";
import { ConfirmModal } from "../../ui/ConfirmModal";
import { Panel } from "../../ui/Panel";
import { RelativeTime } from "../../ui/RelativeTime";
import { ui } from "../../ui/styles";

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

export function SessionDelivery(props: { address: SessionAddress; canManage: boolean }) {
  const posts = createInfiniteQuery(() => ({
    queryKey: ["slack-delivery", props.address.projectID, props.address.environmentID, props.address.sessionID],
    queryFn: ({ pageParam }) => listSlackDelivery(props.address, pageParam),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (page) => page.next_cursor,
    refetchInterval: 5_000,
    retry: false,
  }));
  const rows = createMemo(() => posts.data?.pages.flatMap((page) => page.posts) ?? []);
  const [selected, setSelected] = createSignal<SlackDeliveryPost>();
  const [checking, setChecking] = createSignal(false);
  const [error, setError] = createSignal<string>();
  const check = async (post: SlackDeliveryPost) => {
    setChecking(true); setError(undefined);
    try { await recoverSlackDelivery(props.address, post, "check"); await posts.refetch(); }
    catch (cause) { setError(cause instanceof Error ? cause.message : "Delivery could not be checked."); }
    finally { setChecking(false); }
  };
  return <Panel title="Slack delivery">
    <Show when={posts.isPending}><p class={ui.muted}>Loading Slack delivery...</p></Show>
    <Show when={posts.error}><p class={ui.error} role="alert">Slack delivery could not be loaded.</p></Show>
    <Show when={error()}>{(message) => <p class={ui.error} role="alert">{message()}</p>}</Show>
    <Show when={!posts.isPending && !posts.error && rows().length === 0}><p class={ui.muted}>No Slack delivery issues reported.</p></Show>
    <For each={rows()}>{(post) => <article class="flex flex-col gap-2 border-t border-console-border py-4">
      <h3 class="font-medium">{title(post)}</h3>
      <p class={ui.muted}>{post.role === "intermediate" ? "Progress" : post.role === "request_feedback" ? "Request acknowledgment" : post.role === "question" ? "Question" : post.role === "response" ? "Response" : "Task status"} · Message {post.sequence} · <RelativeTime value={post.created_at} /></p>
      <Show when={!post.payload_expired_at} fallback={<p class={ui.muted}>Message content expired under the Session’s retention policy.</p>}><pre class="max-h-48 overflow-auto whitespace-pre-wrap break-words text-sm">{post.text}</pre></Show>
      <Show when={post.status === "uncertain"}><p>{checkReason(post)}</p></Show>
      <Show when={post.attempt_id && !post.check_paused_at}><p class={ui.muted}>A Slack delivery check is scheduled.</p></Show>
      <Show when={!post.root_known}><p class={ui.muted}>The thread’s first message is unconfirmed. Later messages remain blocked until Slack confirms that original message.</p></Show>
      <Show when={post.disposed_at}>
        <p class={ui.muted}>Remaining parts of this message will not be sent. Agent work continues.</p>
        <Show when={post.stream_state === "uncertain" || post.stream_state === "open"}><p class={ui.muted}>Slack stream termination remains unconfirmed. Evidence is retained until it can be resolved.</p></Show>
      </Show>
      <Show when={post.message_ts && post.confirmed_revision > 0}><p class={ui.muted}>{post.confirmed_revision === post.desired_revision ? "Slack confirmed this message." : "Slack confirmed an earlier version of this message."}</p></Show>
      <Show when={props.canManage && post.attempt_id}>
        <div class="flex flex-wrap gap-2">
          <Show when={post.check_paused_at}><button class={ui.secondaryButton} disabled={checking()} onClick={() => void check(post)}>Check Slack again</button></Show>
          <Show when={post.status === "uncertain" && !post.disposed_at}><button class={ui.secondaryButton} disabled={checking()} onClick={() => setSelected(post)}>Stop delivery</button></Show>
        </div>
      </Show>
      <Show when={!props.canManage && post.attempt_id}><p class={ui.muted}>An organization owner or admin can check again or stop delivery.</p></Show>
    </article>}</For>
    <Show when={posts.hasNextPage}><button class={ui.secondaryButton} disabled={posts.isFetchingNextPage} onClick={() => void posts.fetchNextPage()}>Load more delivery issues</button></Show>
    <Show when={selected()} keyed>{(post) => <ConfirmModal title="Stop delivery of this message?" confirmLabel="Stop delivery" onClose={() => setSelected(undefined)} onConfirm={async () => { await recoverSlackDelivery(props.address, post, "abandon"); await posts.refetch(); }}>
      <p>Stop sending the rest of this message, including its continuation parts. Some content may already be in Slack. This does not stop the Agent.</p>
      <pre class="max-h-40 overflow-auto whitespace-pre-wrap break-words text-sm">{post.text}</pre>
      <Show when={!post.root_known}><p>The original thread must still be confirmed before later messages can be sent. This action will not create a replacement thread.</p></Show>
      <Show when={post.stream_state === "uncertain" || post.stream_state === "open"}><p>Evidence remains while Slack stream termination is unconfirmed; this action does not guarantee content expiry.</p></Show>
    </ConfirmModal>}</Show>
  </Panel>;
}
