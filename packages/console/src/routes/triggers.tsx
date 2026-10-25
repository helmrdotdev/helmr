import { Segmented, SearchInput } from "../ui/Field";
import { Dialog } from "../ui/Dialog";
import { ui } from "../ui/styles";
import { createInfiniteQuery } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { deploymentHref } from "../features/deployments/navigation";
import { agentHref } from "../lib/definitions";
import { loadError } from "../lib/error";
import { useScopeIDs } from "../lib/scope";
import { listTriggers, listTriggerOccurrences } from "../lib/triggers";
import { DataTable, LoadMore, PrimaryLink } from "../ui/DataTable";
import { IDText } from "../ui/IDText";
import { QueryState } from "../ui/QueryState";
import { TableSkeleton } from "../ui/Skeleton";
import { RelativeTime } from "../ui/RelativeTime";

const failure = (error: unknown) => loadError(error, "Could not load triggers.", { forbidden: "You do not have permission to view triggers." });

// Trigger activations across Deployments: every Agent's in the tab's Environment,
// or one Agent's.
export function TriggerList(props: { agent?: string }) {
  const scope = useScopeIDs();
  const agent = () => props.agent ?? "";
  const [view,setView] = createSignal<"current" | "all">("current");
  const [key,setKey] = createSignal("");
  const [activation,setActivation] = createSignal("");
  const triggers = createInfiniteQuery(() => ({
    queryKey: ["triggers", scope().projectID, scope().environmentID, agent(),view(),key()],
    queryFn: ({ pageParam }) => listTriggers(scope(), pageParam || undefined, agent() || undefined,view(),key() || undefined),
    initialPageParam: "",
    getNextPageParam: page => page.next_cursor,
    enabled: !!scope().projectID && !!scope().environmentID,
    retry: false,
  }));
  const items = createMemo(() => triggers.data?.pages.flatMap(page => page.triggers) ?? []);
  const columns = () => [
    "Trigger",
    ...(agent() ? [] : ["Agent"]),
    "Cron",
    "Timezone",
    "Next",
    "Active from",
    "Active until",
    "Deployment",
  ];
  return (<>
    <div class="mb-3 flex flex-wrap gap-3"><Segmented label="Show triggers" value={view()} options={[{value:"current" as const,label:"Current"},{value:"all" as const,label:"All history"}]} onChange={setView} /><SearchInput aria-label="Trigger key" placeholder="Exact trigger key…" value={key()} onInput={e=>setKey(e.currentTarget.value)} /></div>
    <p class="mb-3 text-control text-console-muted">Triggers are defined in code. Change code and deploy to change the schedule.</p>
    <Show when={activation()}>{id=><TriggerOccurrences activation={id()} onClose={()=>setActivation("")} />}</Show>
    <QueryState
      query={triggers}
      pending={<TableSkeleton label="Loading triggers" columns={columns()} minWidth="min-w-230" />}
      error={failure}
      empty={{
        when: (data) => data.pages.every(page => page.triggers.length === 0),
        title: agent() ? "No trigger activations for this agent." : "No trigger activations in this environment.",
      }}
    >
      {() => (
        <DataTable
          columns={columns()}
          minWidth="min-w-230"
          footer={<LoadMore count={items().length} noun={{ one: "trigger", other: "triggers" }} hasMore={triggers.hasNextPage} loading={triggers.isFetchingNextPage} failed={triggers.isFetchNextPageError} onLoadMore={() => void triggers.fetchNextPage()} />}
        >
          <For each={items()}>{trigger => (
            <tr>
              <td><button class={ui.secondaryButton} onClick={()=>setActivation(trigger.id)}>{trigger.trigger_key}</button></td>
              <Show when={!agent()}>
                <td><PrimaryLink href={agentHref(trigger.agent, "triggers")}>{trigger.agent}</PrimaryLink></td>
              </Show>
              <td><code>{trigger.cron.pattern}</code></td>
              <td>{trigger.cron.timezone}</td>
              <td><RelativeTime value={trigger.next_fire_at} /></td>
              <td><RelativeTime value={trigger.active_from} /></td>
              <td><RelativeTime value={trigger.active_until} /></td>
              <td><IDText value={trigger.deployment_id} mode="link" href={deploymentHref(trigger.deployment_id)} /></td>
            </tr>
          )}</For>
        </DataTable>
      )}
    </QueryState></>
  );
}

function TriggerOccurrences(props: {activation:string;onClose:()=>void}) {
 const scope=useScopeIDs();
 const query=createInfiniteQuery(()=>({queryKey:["triggers","occurrences",scope().projectID,scope().environmentID,props.activation],queryFn:({pageParam})=>listTriggerOccurrences(scope(),props.activation,pageParam || undefined),initialPageParam:"",getNextPageParam:page=>page.next_cursor,retry:false}));
 const items=()=>query.data?.pages.flatMap(p=>p.occurrences) ?? [];
 return <Dialog title="Trigger execution history" onClose={props.onClose} class="max-w-180"><QueryState query={query} pending={<TableSkeleton label="Loading occurrences" columns={["Scheduled","Outcome","Reason","Session"]} />} error={failure} empty={{when:data=>data.pages.every(p=>p.occurrences.length===0),title:"No recorded occurrences."}}>{()=> <DataTable columns={["Scheduled","Outcome","Reason","Session"]} footer={<LoadMore count={items().length} noun={{one:"occurrence",other:"occurrences"}} hasMore={query.hasNextPage} loading={query.isFetchingNextPage} failed={query.isFetchNextPageError} onLoadMore={()=>void query.fetchNextPage()} />}><For each={items()}>{item=><tr><td><RelativeTime value={item.scheduled_at} /></td><td>{item.disposition}</td><td>{item.reason ?? "—"}</td><td><Show when={item.session_id} fallback="—">{id=><PrimaryLink href={`/sessions/${encodeURIComponent(id())}`}>Open Session</PrimaryLink>}</Show></td></tr>}</For></DataTable>}</QueryState></Dialog>;
}
