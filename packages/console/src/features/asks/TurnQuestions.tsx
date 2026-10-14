import { createInfiniteQuery } from "@tanstack/solid-query";
import { createMemo, For, Show, createEffect, on } from "solid-js";
import { askConsolePath, listAsks, type SessionAddress } from "../../lib/sessions";
import { LoadMore, PrimaryLink } from "../../ui/DataTable";
import { List, ListItem } from "../../ui/List";
import { QueryState } from "../../ui/QueryState";
import { StatusBadge } from "../../ui/StatusBadge";

export function TurnQuestions(props: { address: SessionAddress; turnID: string; polling: boolean }) {
  const asks = createInfiniteQuery(() => ({
    queryKey: ["turn-asks", props.address, props.turnID],
    queryFn: ({ pageParam }) => listAsks({ ...props.address, turnID: props.turnID }, pageParam || undefined),
    initialPageParam: "", getNextPageParam: page => page.nextCursor,
    retry: false, refetchInterval: props.polling ? 5_000 : false,
  }));
  createEffect(on(() => props.polling, (active, previous) => {
    if (previous && !active) void asks.refetch();
  }));
  const items = createMemo(() => asks.data?.pages.flatMap(page => page.items) ?? []);
  return <QueryState query={asks} error={() => "Questions could not be loaded."}>
    {() => (
      <Show when={items().length > 0}>
        <List
          label="Questions"
          footer={<Show when={asks.hasNextPage}><LoadMore count={items().length} noun={{ one: "question", other: "questions" }} hasMore={asks.hasNextPage} loading={asks.isFetchingNextPage} failed={asks.isFetchNextPageError} onLoadMore={() => void asks.fetchNextPage()} /></Show>}
        >
          <For each={items()}>{ask => <ListItem line class="gap-2">
            <PrimaryLink href={askConsolePath({ ...props.address, turnID: props.turnID, askID: ask.id })}>Question</PrimaryLink>
            <StatusBadge resource="question" status={ask.status} />
          </ListItem>}</For>
        </List>
      </Show>
    )}
  </QueryState>;
}
