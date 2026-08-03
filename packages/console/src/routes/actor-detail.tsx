import { A, useParams, useSearchParams } from "@solidjs/router";
import { createQuery } from "@tanstack/solid-query";
import { createEffect, createMemo, createSignal, For, Show } from "solid-js";
import {
  getActor,
  getActorOutput,
  type ActorAddress,
  type ActorOutputPage,
} from "../lib/actors";
import { ApiError } from "../lib/api";
import { runHref } from "../features/runs/navigation";
import { useScope } from "../lib/scope";
import { cx, statusBadgeClass, ui } from "../ui/styles";

const pageSize = 100;

function actorStatusTone(status: "open" | "closed" | "cancelled" | "failed") {
  if (status === "open") return "active" as const;
  if (status === "closed") return "succeeded" as const;
  if (status === "failed") return "revoked" as const;
  return "expired" as const;
}

function searchParamValue(value: string | string[] | undefined): string {
  return typeof value === "string" ? value.trim() : "";
}

function actorErrorMessage(error: unknown): string {
  if (error instanceof ApiError) return error.message;
  return "Could not load this Actor.";
}

function timeLabel(value: string | undefined): string {
  if (!value) return "—";
  const parsed = new Date(value);
  return Number.isNaN(parsed.getTime()) ? "—" : parsed.toLocaleString();
}

export function ActorDetail() {
  const params = useParams();
  const [searchParams] = useSearchParams();
  const scope = useScope();
  const declaredID = createMemo(() => params["declared_id"]?.trim() ?? "");
  const actorID = createMemo(() => params["actor_id"]?.trim() ?? "");
  const projectID = createMemo(() => searchParamValue(searchParams["project_id"]) || scope.selectedProjectID());
  const environmentID = createMemo(() => searchParamValue(searchParams["environment_id"]) || scope.selectedEnvironmentID());
  const enabled = createMemo(() => !!declaredID() && !!actorID() && !!projectID() && !!environmentID());
  const addressKey = createMemo(() =>
    [projectID(), environmentID(), declaredID(), actorID()].join("\u0000")
  );
  const address = (): ActorAddress => ({
    declaredID: declaredID(),
    actorID: actorID(),
    projectID: projectID(),
    environmentID: environmentID(),
  });

  const actor = createQuery(() => ({
    queryKey: ["actor", declaredID(), actorID(), projectID(), environmentID()],
    queryFn: () => getActor(address()),
    enabled: enabled(),
    retry: false,
  }));
  const initialOutput = createQuery(() => ({
    queryKey: ["actor-output", declaredID(), actorID(), projectID(), environmentID()],
    queryFn: () => getActorOutput(address(), { limit: pageSize }),
    enabled: enabled(),
    retry: false,
  }));
  const [pages, setPages] = createSignal<ActorOutputPage[]>([]);
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
      const page = await getActorOutput(address(), {
        after: current.next_after,
        limit: pageSize,
      });
      if (scopeGeneration === requestedGeneration) {
        setPages((loaded) => [...loaded, page]);
      }
    } catch (error) {
      if (scopeGeneration === requestedGeneration) {
        setPageError(actorErrorMessage(error));
      }
    } finally {
      if (scopeGeneration === requestedGeneration) {
        setLoadingMore(false);
      }
    }
  }

  return (
    <section class={ui.page}>
      <div class={ui.pageHeader}>
        <div>
          <A href="/runs" class={ui.backLink}>Runs</A>
          <div class={ui.pageTitle}>
            <h1 class={ui.h1}>{declaredID() || "Actor"}</h1>
            <Show when={actor.data}>
              {(current) => <span class={statusBadgeClass(actorStatusTone(current().status))}>{current().status}</span>}
            </Show>
          </div>
          <p class="mt-1.5 font-mono text-[12px] text-console-muted">{actorID()}</p>
        </div>
      </div>

      <Show when={actor.isError}>
        <p class={ui.error} role="alert">{actorErrorMessage(actor.error)}</p>
      </Show>
      <Show when={initialOutput.isError}>
        <p class={ui.error} role="alert">{actorErrorMessage(initialOutput.error)}</p>
      </Show>
      <Show when={enabled()} fallback={<p class={ui.error}>Actor ID and environment scope are required.</p>}>
        <Show when={!actor.isPending && !initialOutput.isPending} fallback={<p class={ui.muted}>Loading Actor...</p>}>
          <Show when={actor.data}>
            {(current) => (
              <div class="grid grid-cols-[minmax(0,1fr)_300px] items-start gap-3.5 max-[960px]:grid-cols-1">
                <section class="border border-console-border bg-console-surface p-4">
                  <h2 class={cx(ui.h2, "mb-3")}>Output</h2>
                  <Show when={records().length > 0} fallback={<p class={ui.emptyState}>No output.</p>}>
                    <ol class="m-0 list-none border border-console-border p-0">
                      <For each={records()}>
                        {(record) => (
                          <li class="border-b border-console-border-soft px-3 py-2.5 last:border-b-0">
                            <div class="flex flex-wrap items-center justify-between gap-2 font-mono text-[10.5px] text-console-subtle">
                              <span>#{record.sequence} · {record.content_type}</span>
                              <span>{timeLabel(record.created_at)}</span>
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
                    {(message) => <p class={ui.error} role="alert">{message()}</p>}
                  </Show>
                </section>

                <aside class="sticky top-13.5 flex flex-col gap-3 max-[960px]:static">
                  <section class="border border-console-border bg-console-surface px-4 py-3.5">
                    <h3 class={cx(ui.h3, "mb-3.5")}>Actor details</h3>
                    <dl class="m-0 grid gap-2.5 [&>div]:grid [&>div]:gap-0.75 [&_dt]:font-mono [&_dt]:text-[10px] [&_dt]:uppercase [&_dt]:text-console-subtle [&_dd]:m-0 [&_dd]:break-words [&_dd]:text-[12px]">
                      <div><dt>ID</dt><dd><code>{current().id}</code></dd></div>
                      <Show when={current().key}><div><dt>Key</dt><dd>{current().key}</dd></div></Show>
                      <div><dt>Status</dt><dd>{current().status}</dd></div>
                      <div><dt>Created</dt><dd>{timeLabel(current().created_at)}</dd></div>
                      <div><dt>Updated</dt><dd>{timeLabel(current().updated_at)}</dd></div>
                      <Show when={current().current_run_id}>
                        {(runID) => <div><dt>Current Run</dt><dd><A class="text-console-accent" href={runHref(runID(), projectID(), environmentID())}>{runID()}</A></dd></div>}
                      </Show>
                      <Show when={current().failure}>
                        {(failure) => (
                          <>
                            <div><dt>Failure</dt><dd>{failure().code}</dd></div>
                            <div><dt>Failure Run</dt><dd><A class="text-console-accent" href={runHref(failure().run_id, projectID(), environmentID())}>{failure().run_id}</A></dd></div>
                          </>
                        )}
                      </Show>
                    </dl>
                  </section>
                </aside>
              </div>
            )}
          </Show>
        </Show>
      </Show>
    </section>
  );
}
