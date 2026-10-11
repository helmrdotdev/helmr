import { createInfiniteQuery } from "@tanstack/solid-query";
import { For, Show, createEffect, on } from "solid-js";
import { askConsolePath, listAsks, type SessionAddress } from "../../lib/sessions";
import { ui } from "../../ui/styles";

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
  return <div>
    <Show when={asks.isError}><p role="alert">Questions could not be loaded.</p></Show>
    <For each={asks.data?.pages.flatMap(page => page.asks)}>{ask => <p><a class="underline" href={askConsolePath({ ...props.address, turnID: props.turnID, askID: ask.id })}>Question — {ask.status}</a></p>}</For>
    <Show when={asks.hasNextPage}><button class={ui.secondaryButton} disabled={asks.isFetchingNextPage} onClick={() => void asks.fetchNextPage()}>Load more questions</button></Show>
  </div>;
}
