import { A, useParams, useSearchParams } from "@solidjs/router";
import { createQuery } from "@tanstack/solid-query";
import { createEffect, createMemo, createSignal, For, Show } from "solid-js";
import {
  getSession,
  getSessionOutput,
  type SessionAddress,
  type SessionOutputPage,
} from "../lib/sessions";
import { ApiError } from "../lib/api";
import { deploymentHref } from "../features/deployments/navigation";
import { runHref } from "../features/runs/navigation";
import { useScope } from "../lib/scope";
import { IDText } from "../ui/IDText";
import { PageHeader } from "../ui/PageHeader";
import { DetailItem, DetailList, Panel } from "../ui/Panel";
import { RelativeTime } from "../ui/RelativeTime";
import { StatePanel } from "../ui/StatePanel";
import { StatusBadge } from "../ui/StatusBadge";
import { ui } from "../ui/styles";

const pageSize = 100;

function searchParamValue(value: string | string[] | undefined): string {
  return typeof value === "string" ? value.trim() : "";
}

function sessionErrorMessage(error: unknown): string {
  if (error instanceof ApiError) return error.message;
  return "Could not load this Session.";
}

export function SessionDetail() {
  const params = useParams();
  const [searchParams] = useSearchParams();
  const scope = useScope();
  const sessionID = createMemo(() => params["session_id"]?.trim() ?? "");
  const projectID = createMemo(() => searchParamValue(searchParams["project_id"]) || scope.selectedProjectID());
  const environmentID = createMemo(() => searchParamValue(searchParams["environment_id"]) || scope.selectedEnvironmentID());
  const enabled = createMemo(() => !!sessionID() && !!projectID() && !!environmentID());
  const addressKey = createMemo(() =>
    [projectID(), environmentID(), sessionID()].join("\u0000")
  );
  const address = (): SessionAddress => ({
    sessionID: sessionID(),
    projectID: projectID(),
    environmentID: environmentID(),
  });

  const session = createQuery(() => ({
    queryKey: ["session", sessionID(), projectID(), environmentID()],
    queryFn: () => getSession(address()),
    enabled: enabled(),
    retry: false,
  }));
  const initialOutput = createQuery(() => ({
    queryKey: ["session-output", sessionID(), projectID(), environmentID()],
    queryFn: () => getSessionOutput(address(), { limit: pageSize }),
    enabled: enabled(),
    retry: false,
  }));
  const [pages, setPages] = createSignal<SessionOutputPage[]>([]);
  const [loadingMore, setLoadingMore] = createSignal(false);
  const [pageError, setPageError] = createSignal<string | null>(null);
  let scopeGeneration = 0;

  createEffect(() => {
    addressKey();
    scopeGeneration += 1;
    setPages([]);
    setLoadingMore(false);
    setPageError(null);
  });

  const records = createMemo(() => [
    ...(initialOutput.data?.records ?? []),
    ...pages().flatMap((page) => page.records),
  ]);
  const lastPage = createMemo(() => {
    const loaded = pages();
    return loaded.length > 0 ? loaded[loaded.length - 1] : initialOutput.data;
  });

  async function loadMore() {
    const current = lastPage();
    if (!current?.has_more || loadingMore()) return;
    const requestedGeneration = scopeGeneration;
    setLoadingMore(true);
    setPageError(null);
    try {
      const page = await getSessionOutput(address(), {
        after: current.next_after,
        limit: pageSize,
      });
      if (scopeGeneration === requestedGeneration) {
        setPages((loaded) => [...loaded, page]);
      }
    } catch (error) {
      if (scopeGeneration === requestedGeneration) {
        setPageError(sessionErrorMessage(error));
      }
    } finally {
      if (scopeGeneration === requestedGeneration) {
        setLoadingMore(false);
      }
    }
  }

  return (
    <section class={ui.page}>
      <PageHeader
        title={session.data?.actor_id ?? "Session"}
        back={{ href: "/sessions", label: "Sessions" }}
        badge={<Show when={session.data}>{(current) => <StatusBadge resource="session" status={current().status} />}</Show>}
        subtitle={<IDText value={sessionID()} full />}
      />

      <Show when={session.isError}>
        <StatePanel error={sessionErrorMessage(session.error)} />
      </Show>
      <Show when={initialOutput.isError}>
        <StatePanel error={sessionErrorMessage(initialOutput.error)} />
      </Show>
      <Show when={enabled()} fallback={<StatePanel error="Session ID and environment scope are required." />}>
        <Show when={!session.isPending && !initialOutput.isPending} fallback={<StatePanel loading="Loading Session..." />}>
          <Show when={session.data}>
            {(current) => (
              <div class="grid grid-cols-[minmax(0,1fr)_300px] items-start gap-3.5 max-[960px]:grid-cols-1">
                <Panel title="Output">
                  <Show when={records().length > 0} fallback={<StatePanel empty="No output." />}>
                    <ol class="m-0 list-none border border-console-border p-0">
                      <For each={records()}>
                        {(record) => (
                          <li class="border-b border-console-border-soft px-3 py-2.5 last:border-b-0">
                            <div class="flex flex-wrap items-center justify-between gap-2 font-mono text-[10.5px] text-console-subtle">
                              <span>#{record.sequence} · {record.content_type}</span>
                              <RelativeTime value={record.created_at} />
                            </div>
                            <pre class="my-2 whitespace-pre-wrap break-words font-mono text-[12px] text-console-text">
                              {JSON.stringify(record.data, null, 2)}
                            </pre>
                            <div class="font-mono text-[10.5px] text-console-subtle">
                              <A
                                class="text-console-accent"
                                href={runHref(record.provenance.run_id, projectID(), environmentID())}
                              >
                                {record.provenance.run_id}
                              </A>
                              {" · "}attempt {record.provenance.attempt_number}
                              {" · "}{record.provenance.deployment_id}
                            </div>
                          </li>
                        )}
                      </For>
                    </ol>
                  </Show>
                  <Show when={lastPage()?.has_more}>
                    <button class={ui.secondaryButton} disabled={loadingMore()} onClick={loadMore}>
                      {loadingMore() ? "Loading..." : "Load more output"}
                    </button>
                  </Show>
                  <Show when={pageError()}>
                    {(message) => <StatePanel error={message()} />}
                  </Show>
                </Panel>

                <DetailList title="Session details">
                  <DetailItem label="ID"><IDText value={current().id} full /></DetailItem>
                  <DetailItem label="Actor ID"><code>{current().actor_id}</code></DetailItem>
                  <DetailItem label="Deployment">
                    <IDText value={current().deployment_id} full href={deploymentHref(current().deployment_id)} />
                  </DetailItem>
                  <Show when={current().key}>{(key) => <DetailItem label="Key"><code>{key()}</code></DetailItem>}</Show>
                  <DetailItem label="Status"><StatusBadge resource="session" status={current().status} /></DetailItem>
                  <DetailItem label="Created"><RelativeTime value={current().created_at} /></DetailItem>
                  <DetailItem label="Updated"><RelativeTime value={current().updated_at} /></DetailItem>
                  <Show when={current().current_run_id}>
                    {(runID) => (
                      <DetailItem label="Current Run">
                        <IDText value={runID()} full href={runHref(runID(), projectID(), environmentID())} />
                      </DetailItem>
                    )}
                  </Show>
                  <Show when={current().failure}>
                    {(failure) => (
                      <>
                        <DetailItem label="Failure">{failure().code}</DetailItem>
                        <Show when={failure().details.run_id}>
                          {(runID) => (
                            <DetailItem label="Failure Run">
                              <IDText value={runID()} full href={runHref(runID(), projectID(), environmentID())} />
                            </DetailItem>
                          )}
                        </Show>
                      </>
                    )}
                  </Show>
                </DetailList>
              </div>
            )}
          </Show>
        </Show>
      </Show>
    </section>
  );
}
