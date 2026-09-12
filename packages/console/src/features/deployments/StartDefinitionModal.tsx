import { useNavigate } from "@solidjs/router";
import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, Show } from "solid-js";
import { ApiError } from "../../lib/api";
import { startActor, startTask } from "../../lib/definitions";
import { sessionConsolePath } from "../../lib/sessions";
import { listWorkspaces, type Workspace } from "../../lib/workspaces";
import { Modal } from "../../ui/Modal";
import { Select, type SelectOption } from "../../ui/Select";
import { formatID } from "../../ui/id";
import { StatePanel } from "../../ui/StatePanel";
import { ui } from "../../ui/styles";
import { runHref } from "../runs/navigation";
import { CreateWorkspaceModal } from "../workspaces/CreateWorkspaceModal";

export type StartKind = "task" | "actor";

function startErrorMessage(error: unknown, kind: StartKind): string {
  if (error instanceof ApiError && error.code === "forbidden") return "You do not have permission to do this.";
  if (error instanceof ApiError) return error.message;
  return kind === "task" ? "Could not start this Task." : "Could not start this Actor.";
}

function parseOptionalJSON(raw: string, label: string): { value?: unknown; error?: string } {
  if (raw.trim() === "") return {};
  try {
    return { value: JSON.parse(raw) };
  } catch {
    return { error: `${label} must be valid JSON.` };
  }
}

export function StartDefinitionModal(props: {
  kind: StartKind;
  definitionID: string;
  projectID: string;
  environmentID: string;
  onClose: () => void;
}) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const scope = () => ({ projectID: props.projectID, environmentID: props.environmentID });
  const workspaces = createQuery(() => ({
    queryKey: ["workspaces", "picker", props.projectID, props.environmentID],
    queryFn: () => listWorkspaces(scope(), { limit: 100 }),
    retry: false,
  }));
  const workspaceOptions = createMemo<SelectOption<string>[]>(() =>
    (workspaces.data?.workspaces ?? [])
      .filter((workspace) => workspace.status === "available")
      .map((workspace) => ({
        value: workspace.id,
        label: workspace.key ?? formatID(workspace.id),
        hint: workspace.sandbox_id,
      })),
  );

  const [workspaceID, setWorkspaceID] = createSignal("");
  const [creatingWorkspace, setCreatingWorkspace] = createSignal(false);
  const [payload, setPayload] = createSignal("");
  const [key, setKey] = createSignal("");
  const [input, setInput] = createSignal("");
  const [submitting, setSubmitting] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);
  const selectedWorkspace = createMemo(() => {
    const chosen = workspaceID();
    if (chosen && workspaceOptions().some((option) => option.value === chosen)) return chosen;
    return workspaceOptions()[0]?.value ?? "";
  });
  const title = () => (props.kind === "task" ? `Start Task ${props.definitionID}` : `Start Actor ${props.definitionID}`);

  const submit = async (event: Event) => {
    event.preventDefault();
    const workspace = selectedWorkspace();
    if (!workspace) {
      setError("Choose a Workspace.");
      return;
    }
    setError(null);
    setSubmitting(true);
    try {
      if (props.kind === "task") {
        const parsed = parseOptionalJSON(payload(), "Payload");
        if (parsed.error) {
          setError(parsed.error);
          return;
        }
        const result = await startTask(props.definitionID, scope(), {
          workspace: { id: workspace },
          ...("value" in parsed ? { payload: parsed.value } : {}),
          idempotency_key: crypto.randomUUID(),
        });
        await queryClient.invalidateQueries({ queryKey: ["runs"] });
        props.onClose();
        navigate(runHref(result.run_id, props.projectID, props.environmentID));
      } else {
        const parsed = parseOptionalJSON(input(), "Initial input");
        if (parsed.error) {
          setError(parsed.error);
          return;
        }
        const nextKey = key().trim();
        const result = await startActor(props.definitionID, scope(), {
          workspace: { id: workspace },
          ...(nextKey ? { key: nextKey } : {}),
          ...("value" in parsed ? { input: parsed.value } : {}),
          idempotency_key: crypto.randomUUID(),
        });
        await queryClient.invalidateQueries({ queryKey: ["sessions"] });
        props.onClose();
        navigate(sessionConsolePath(result.session_id, props.projectID, props.environmentID));
      }
    } catch (cause) {
      setError(startErrorMessage(cause, props.kind));
    } finally {
      setSubmitting(false);
    }
  };

  const onWorkspaceCreated = async (workspace: Workspace) => {
    await queryClient.invalidateQueries({ queryKey: ["workspaces"] });
    setWorkspaceID(workspace.id);
  };

  return (
    <>
      <Modal title={title()} onClose={props.onClose} closeDisabled={submitting()}>
        <form onSubmit={submit}>
          <p class={ui.modalIntro}>
            {props.kind === "task"
              ? "Starts a Run of this Task on the current Deployment in the selected Workspace."
              : "Opens a Session of this Actor on the current Deployment in the selected Workspace."}
          </p>
          <Show when={!workspaces.isPending} fallback={<StatePanel loading="Loading Workspaces..." />}>
            <Show when={!workspaces.isError} fallback={<StatePanel error={startErrorMessage(workspaces.error, props.kind)} />}>
              <div class={ui.field}>
                <span>Workspace</span>
                <div class="flex items-center gap-1.5">
                  <div class="min-w-0 flex-1">
                    <Select<string>
                      value={selectedWorkspace()}
                      options={workspaceOptions()}
                      onChange={setWorkspaceID}
                      ariaLabel="Workspace"
                      placeholder="No available Workspaces"
                      disabled={submitting() || workspaceOptions().length === 0}
                    />
                  </div>
                  <button type="button" class={ui.secondaryButton} disabled={submitting()} onClick={() => setCreatingWorkspace(true)}>
                    Create Workspace
                  </button>
                </div>
              </div>
            </Show>
          </Show>
          <Show when={props.kind === "task"}>
            <label class={ui.field}>
              <span>Payload (JSON, optional)</span>
              <textarea
                class={`${ui.textarea} font-mono`}
                rows={5}
                value={payload()}
                onInput={(event) => setPayload(event.currentTarget.value)}
                placeholder="{}"
                spellcheck={false}
                disabled={submitting()}
              />
            </label>
          </Show>
          <Show when={props.kind === "actor"}>
            <label class={ui.field}>
              <span>Key (optional)</span>
              <input
                type="text"
                class={ui.input}
                value={key()}
                onInput={(event) => setKey(event.currentTarget.value)}
                placeholder="customer-42"
                autocomplete="off"
                spellcheck={false}
                disabled={submitting()}
              />
            </label>
            <label class={ui.field}>
              <span>Initial input (JSON, optional)</span>
              <textarea
                class={`${ui.textarea} font-mono`}
                rows={5}
                value={input()}
                onInput={(event) => setInput(event.currentTarget.value)}
                placeholder="{}"
                spellcheck={false}
                disabled={submitting()}
              />
            </label>
          </Show>
          <Show when={error()}>
            <p class={ui.fieldError} role="alert">{error()}</p>
          </Show>
          <div class={ui.modalActions}>
            <button type="button" class={ui.secondaryButton} disabled={submitting()} onClick={props.onClose}>
              Cancel
            </button>
            <button type="submit" class={ui.button} disabled={submitting() || !selectedWorkspace()}>
              {submitting() ? "Starting..." : "Start"}
            </button>
          </div>
        </form>
      </Modal>
      <Show when={creatingWorkspace()}>
        <CreateWorkspaceModal
          projectID={props.projectID}
          environmentID={props.environmentID}
          onClose={() => setCreatingWorkspace(false)}
          onCreated={onWorkspaceCreated}
        />
      </Show>
    </>
  );
}
