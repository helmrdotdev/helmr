import { createInfiniteQuery } from "@tanstack/solid-query";
import { createSignal, For, Show } from "solid-js";
import { request, postJson } from "../../lib/api";
import { useScopeIDs } from "../../lib/scope";
import { ConfirmDialog } from "../../ui/Dialog";
import { IDText } from "../../ui/IDText";
import { Notice } from "../../ui/Notice";
import { ui } from "../../ui/styles";

type Command = { id: string; computer_id: string; status: string; created_at: string };
type Page = { commands: Command[]; next_cursor?: string; can_cancel: boolean };
export function RelatedCommands(props: { user: string }) {
  const scope = useScopeIDs();
  const path = () => `/api/projects/${encodeURIComponent(scope().projectID)}/environments/${encodeURIComponent(scope().environmentID)}`;
  const commands = createInfiniteQuery(() => ({
    queryKey: ["related-commands", scope().projectID, scope().environmentID, props.user],
    queryFn: ({ pageParam }) => request<Page>(`${path()}/related-commands?${new URLSearchParams({ user: props.user, ...(pageParam ? { cursor: pageParam } : {}) })}`),
    initialPageParam: "", getNextPageParam: (page: Page) => page.next_cursor,
    enabled: !!scope().projectID && !!scope().environmentID && !!props.user, retry: false,
  }));
  const [stopping, setStopping] = createSignal<{ command: Command; path: string }>();
  return <section class="mt-6">
    <h2>Direct commands</h2>
    <p>Active commands requested by this person. Stopping a command is separate from revoking access.</p>
    <Show when={commands.error}><Notice tone="danger">{commands.error instanceof Error ? commands.error.message : "Unable to read commands. Computer read permission is required."}</Notice></Show>
    <Show when={commands.isPending}><p>Loading commands…</p></Show>
    <For each={commands.data?.pages}>{page => <For each={page.commands}>{command => <div class="my-2 flex flex-wrap items-center gap-3">
      <IDText value={command.id} /><span>{command.status}</span><IDText value={command.computer_id} mode="link" href={`/computers/${encodeURIComponent(command.computer_id)}`} />
      <Show when={page.can_cancel && ["pending", "starting", "running"].includes(command.status)}><button class={ui.secondaryButton} onClick={() => setStopping({ command, path: path() })}>Stop command…</button></Show>
    </div>}</For>}</For>
    <Show when={commands.data && commands.data.pages.every(page => page.commands.length === 0)}><p>No active direct commands in this environment.</p></Show>
    <Show when={commands.hasNextPage}><button class={ui.secondaryButton} disabled={commands.isFetchingNextPage} onClick={() => void commands.fetchNextPage()}>Load more commands</button></Show>
    <Show when={stopping()} keyed>{target => <ConfirmDialog title="Stop command" confirmLabel="Stop command" tone="danger" target={<IDText value={target.command.id} />} onClose={() => setStopping(undefined)} onConfirm={async () => { await postJson(`${target.path}/commands/${target.command.id}/cancel`, {}); await commands.refetch(); }}>Request this command to stop. Other work and the person's access remain unchanged. Completed results remain available.</ConfirmDialog>}</Show>
  </section>;
}
