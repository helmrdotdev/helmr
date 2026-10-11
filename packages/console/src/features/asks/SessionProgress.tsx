import { createInfiniteQuery } from "@tanstack/solid-query";
import { For, Show, createEffect, on } from "solid-js";
import { normalizeContent } from "../../../../../sdk/typescript/src/content";
import { ApiError } from "../../lib/api";
import { getSessionEvents, type SessionAddress } from "../../lib/sessions";
import { Panel } from "../../ui/Panel";
import { ui } from "../../ui/styles";
import { ContentView } from "./Question";
import { messageDeliveryNotice } from "../../lib/sessions";

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
  return <Panel title="Progress">
    <p class={ui.muted}>Progress is shown in publication order. It remains separate from a completed response.</p>
    <Show when={events.isError}><p role="alert">Progress could not be loaded.</p></Show>
    <Show when={(events.data?.pages[0]?.retained_after ?? 0) > 0}><p>Earlier events have expired.</p></Show>
    <For each={events.data?.pages.flatMap(page => page.notices)}>{event => <article class="py-3" role="status"><p>{event.text}</p><p class={ui.muted}>Turn {event.turn_id}</p></article>}</For>
    <For each={events.data?.pages.flatMap(page => page.output)}>{event => <article class="py-3" aria-label={`Progress ${event.sequence}`}>
      <p class={ui.muted}>Turn {event.turn_id}</p><ContentView content={event.content} />
    </article>}</For>
    <Show when={events.hasNextPage}><button class={ui.secondaryButton} disabled={events.isFetchingNextPage} onClick={() => void events.fetchNextPage()}>Load more progress</button></Show>
  </Panel>;
}
