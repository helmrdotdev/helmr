import { createInfiniteQuery } from "@tanstack/solid-query";
import { createMemo, For, Show, createEffect, on } from "solid-js";
import { normalizeContent } from "../../../../../sdk/typescript/src/content";
import { ApiError } from "../../lib/api";
import { getSessionEvents, type SessionAddress } from "../../lib/sessions";
import { LoadMore } from "../../ui/DataTable";
import { IDText } from "../../ui/IDText";
import { List, ListItem } from "../../ui/List";
import { Notice } from "../../ui/Notice";
import { Panel } from "../../ui/Panel";
import { QueryState } from "../../ui/QueryState";
import { ui } from "../../ui/styles";
import { ContentView } from "./Question";
import { messageDeliveryNotice } from "../../lib/sessions";

function TurnRef(props: { id: string | null }) {
  return <p class={ui.muted}>Turn <IDText value={props.id ?? ""} mode="link" href={props.id ? `#turn-${props.id}` : undefined} /></p>;
}

export function SessionProgress(props: { address: SessionAddress; polling: boolean }) {
  const events = createInfiniteQuery(() => ({
    queryKey: ["session-progress", props.address],
    queryFn: async ({ pageParam }) => {
      let page;
      try { page = await getSessionEvents(props.address, { after: pageParam, limit: 100 }); }
      catch (error) {
        const floor = error instanceof ApiError && error.code === "cursor_expired" ? error.details?.["retained_after"] : undefined;
        if (typeof floor !== "number" || !Number.isSafeInteger(floor) || floor < 0) throw error;
        page = await getSessionEvents(props.address, { after: floor, limit: 100 });
      }
      return { ...page, notices: page.records.flatMap(event => { const text = messageDeliveryNotice(event); return text ? [{ ...event, text }] : [] }), output: page.records.filter(event => event.kind === "turn.output").map(event => ({ ...event, content: normalizeContent(event.data, false) })) };
    },
    initialPageParam: 0,
    getNextPageParam: page => page.has_more ? page.next_after : undefined,
    retry: false, refetchInterval: props.polling ? 5_000 : false,
  }));
  createEffect(on(() => props.polling, (active, previous) => {
    if (previous && !active) void events.refetch();
  }));
  const notices = createMemo(() => events.data?.pages.flatMap(page => page.notices) ?? []);
  const output = createMemo(() => events.data?.pages.flatMap(page => page.output) ?? []);
  return <Panel title="Progress">
    <p class={ui.muted}>Progress is shown in publication order. It remains separate from a completed response.</p>
    <QueryState query={events} pending="Loading progress..." error={() => "Progress could not be loaded."}>
      {() => <>
        <Show when={(events.data?.pages[0]?.retained_after ?? 0) > 0}><Notice tone="info">Earlier events have expired.</Notice></Show>
        <For each={notices()}>{event => <Notice tone="warning"><p class="m-0">{event.text}</p><TurnRef id={event.turn_id} /></Notice>}</For>
        <Show when={output().length > 0 || events.hasNextPage}>
          <List
            label="Progress"
            footer={<LoadMore count={output().length} noun="progress" hasMore={events.hasNextPage} loading={events.isFetchingNextPage} failed={events.isFetchNextPageError} onLoadMore={() => void events.fetchNextPage()} />}
          >
            <For each={output()}>{event => <ListItem>
              <article class={ui.windowStack} aria-label={`Progress ${event.sequence}`}>
                <TurnRef id={event.turn_id} /><ContentView content={event.content} />
              </article>
            </ListItem>}</For>
          </List>
        </Show>
      </>}
    </QueryState>
  </Panel>;
}
