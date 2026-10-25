import { Show } from "solid-js";
import type { Session } from "../../lib/sessions";
import { Icon } from "../../ui/Icon";

// Where a Session started, as far as the Session says: a Slack channel or DM,
// another Session, or the key it was started with. Anything else came from the
// CLI, the API or a trigger, which the Session does not tell apart, so it says
// so instead of guessing. The icon helps scanning; the words say it. With `id`
// the Slack conversation ID follows.
export function SessionOrigin(props: { session: Pick<Session, "slack_channel_id" | "key" | "parent_session_id" | "requester_session_id">; id?: boolean }) {
  return (
    <span class="inline-flex min-w-0 items-center gap-1.5">
      <Show
        when={props.session.slack_channel_id}
        fallback={
          <Show when={!props.session.parent_session_id && !props.session.requester_session_id} fallback={<span>Another session</span>}>
            <Show when={props.session.key} fallback={<span class="text-console-muted">CLI, API or trigger</span>}>
              {key => <span class="truncate">key <code>{key()}</code></span>}
            </Show>
          </Show>
        }
      >
        {channel => <>
          <span class="text-console-subtle"><Icon name={channel().startsWith("D") ? "slack-dm" : "slack-channel"} /></span>
          <span class="truncate">{channel().startsWith("D") ? "Slack DM" : "Slack channel"}<Show when={props.id}> · <code>{channel()}</code></Show></span>
        </>}
      </Show>
    </span>
  );
}
