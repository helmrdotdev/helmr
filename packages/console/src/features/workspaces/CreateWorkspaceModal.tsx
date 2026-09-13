import { createQuery } from "@tanstack/solid-query";
import { createMemo, createSignal, Index, Show } from "solid-js";
import { ApiError } from "../../lib/api";
import { listSandboxes } from "../../lib/definitions";
import { listSecrets } from "../../lib/secrets";
import { createWorkspace, type Workspace, type WorkspaceSecret } from "../../lib/workspaces";
import { Modal } from "../../ui/Modal";
import { Select, type SelectOption } from "../../ui/Select";
import { StatePanel } from "../../ui/StatePanel";
import { ui } from "../../ui/styles";

type PlacementKind = "protected" | "raw" | "file";

type PlacementRow = {
  name: string;
  kind: PlacementKind;
  target: string;
  origins: string;
};

const PLACEMENT_KINDS: SelectOption<PlacementKind>[] = [
  { value: "protected", label: "Protected env" },
  { value: "raw", label: "Raw env" },
  { value: "file", label: "Raw file" },
];

function createErrorMessage(error: unknown): string {
  if (error instanceof ApiError && error.code === "forbidden") return "You do not have permission to create Workspaces.";
  if (error instanceof ApiError) return error.message;
  return "Could not create this Workspace.";
}

function placementsToSecrets(rows: readonly PlacementRow[]): WorkspaceSecret[] {
  return rows.map((row) => {
    if (row.kind === "file") return { secret: row.name, file: { path: row.target.trim() } };
    if (row.kind === "raw") return { secret: row.name, env: { name: row.target.trim(), mode: "raw" } };
    const origins = row.origins.split(/[\s,]+/).filter(Boolean).map((value) => {
      let url: URL;
      try { url = new URL(value); } catch { throw new Error("Use exact HTTPS origins, such as https://api.example.com."); }
      if (url.protocol !== "https:" || url.username || url.password || url.search || url.hash || url.pathname !== "/" || value.includes("?") || value.includes("#")) throw new Error("Use exact HTTPS origins, such as https://api.example.com.");
      return url.origin;
    });
    return { secret: row.name, env: { name: row.target.trim(), mode: "protected", allowed_origins: [...new Set(origins)].sort() } };
  });
}

export function CreateWorkspaceModal(props: {
  projectID: string;
  environmentID: string;
  onClose: () => void;
  onCreated: (workspace: Workspace) => Promise<void>;
}) {
  const scope = () => ({ projectID: props.projectID, environmentID: props.environmentID });
  const sandboxes = createQuery(() => ({
    queryKey: ["sandboxes", "current", props.projectID, props.environmentID],
    queryFn: () => listSandboxes({ ...scope(), limit: 100 }),
    retry: false,
  }));
  const secrets = createQuery(() => ({
    queryKey: ["secrets", props.projectID, props.environmentID],
    queryFn: () => listSecrets(props.projectID, props.environmentID),
    retry: false,
  }));
  const sandboxOptions = createMemo<SelectOption<string>[]>(() =>
    (sandboxes.data?.sandboxes ?? []).map((sandbox) => ({ value: sandbox.id, label: sandbox.id })),
  );
  const secretOptions = createMemo<SelectOption<string>[]>(() =>
    (secrets.data?.secrets ?? [])
      .filter((secret) => secret.status === "active")
      .map((secret) => ({ value: secret.name, label: secret.name })),
  );

  const [sandboxID, setSandboxID] = createSignal("");
  const [key, setKey] = createSignal("");
  const [placements, setPlacements] = createSignal<PlacementRow[]>([]);
  const [submitting, setSubmitting] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  const selectedSandbox = createMemo(() => sandboxID() || sandboxOptions()[0]?.value || "");
  const placementsComplete = createMemo(() =>
    placements().every((row) => row.name !== "" && row.target.trim() !== ""),
  );

  const addPlacement = () => {
    const first = secretOptions()[0]?.value ?? "";
    setPlacements((rows) => [...rows, { name: first, kind: "protected", target: "", origins: "" }]);
  };
  const updatePlacement = (index: number, patch: Partial<PlacementRow>) => {
    setPlacements((rows) => rows.map((row, position) => (position === index ? { ...row, ...patch } : row)));
  };
  const removePlacement = (index: number) => {
    setPlacements((rows) => rows.filter((_, position) => position !== index));
  };

  let attempt: { fingerprint: string; idempotencyKey: string; workspace?: Workspace } | undefined;

  const submit = async (event: Event) => {
    event.preventDefault();
    const sandbox = selectedSandbox();
    if (!sandbox) {
      setError("Choose a Sandbox.");
      return;
    }
    if (!placementsComplete()) {
      setError("Every Secret placement needs a Secret and an env var name or file path.");
      return;
    }
    setError(null);
    setSubmitting(true);
    let currentAttempt: typeof attempt;
    try {
      const nextKey = key().trim();
      let secretRows: WorkspaceSecret[];
      try { secretRows = placementsToSecrets(placements()); } catch {
        setError("Use exact HTTPS origins, such as https://api.example.com.");
        return;
      }
      const capturedScope = scope();
      const input = {
        ...(nextKey ? { key: nextKey } : {}),
        ...(secretRows.length > 0 ? { secrets: secretRows } : {}),
      };
      const fingerprint = JSON.stringify({ scope: capturedScope, sandbox, input });
      if (attempt?.fingerprint !== fingerprint) attempt = { fingerprint, idempotencyKey: crypto.randomUUID() };
      currentAttempt = attempt;
      const workspace = currentAttempt.workspace ?? await createWorkspace(sandbox, capturedScope, {
        ...input, idempotency_key: currentAttempt.idempotencyKey,
      });
      currentAttempt.workspace = workspace;
      await props.onCreated(workspace);
      props.onClose();
    } catch (cause) {
      setError(currentAttempt?.workspace
        ? "Workspace was created, but the view could not be updated. Retry without changing inputs to continue."
        : cause instanceof ApiError
          ? createErrorMessage(cause)
          : "Workspace creation could not be confirmed. Retry without changing inputs to continue with the same request.");
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Modal title="Create Workspace" onClose={props.onClose} closeDisabled={submitting()}>
      <form onSubmit={submit}>
        <p class={ui.modalIntro}>
          Create from a Sandbox of the current Deployment. Secret bindings are fixed at creation.
        </p>
        <Show when={!sandboxes.isPending} fallback={<StatePanel loading="Loading Sandboxes..." />}>
          <Show when={!sandboxes.isError} fallback={<StatePanel error={createErrorMessage(sandboxes.error)} />}>
            <Show
              when={sandboxOptions().length > 0}
              fallback={<StatePanel empty="The current Deployment declares no Sandboxes." hint="Deploy a bundle with a Sandbox to create Workspaces." />}
            >
              <label class={ui.field}>
                <span>Sandbox</span>
                <Select<string>
                  value={selectedSandbox()}
                  options={sandboxOptions()}
                  onChange={setSandboxID}
                  ariaLabel="Sandbox"
                  disabled={submitting()}
                />
              </label>
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
              <fieldset class={ui.fieldSet}>
                <legend class={ui.fieldLegend}>Secret placements</legend>
                <p class={ui.muted}>Protected env replaces header placeholders for proxy-compatible clients at approved HTTPS origins. Raw placements deliver the value.</p>
                <Show when={secrets.isError}><StatePanel error="Could not load Secrets." /></Show>
                <Show when={placements().length > 0} fallback={<p class={ui.muted}>No Secrets are attached.</p>}>
                  <div class="grid gap-1.5">
                    <Index each={placements()}>
                      {(row, index) => (
                        <div class="grid grid-cols-[minmax(0,1fr)_140px_minmax(0,1fr)_auto] items-center gap-1.5">
                          <Select<string>
                            value={row().name}
                            options={secretOptions()}
                            onChange={(name) => updatePlacement(index, { name })}
                            ariaLabel="Secret"
                            placeholder="Secret"
                            disabled={submitting()}
                          />
                          <Select<PlacementKind>
                            value={row().kind}
                            options={PLACEMENT_KINDS}
                            onChange={(kind) => updatePlacement(index, { kind })}
                            ariaLabel="Placement"
                            disabled={submitting()}
                          />
                          <input
                            type="text"
                            class={ui.input}
                            value={row().target}
                            onInput={(event) => updatePlacement(index, { target: event.currentTarget.value })}
                            placeholder={row().kind !== "file" ? "API_TOKEN" : "/run/secrets/token"}
                            aria-label={row().kind !== "file" ? "Env var name" : "File path"}
                            autocomplete="off"
                            spellcheck={false}
                            disabled={submitting()}
                          />
                          <button
                            type="button"
                            class={ui.ghostButton}
                            aria-label="Remove placement"
                            disabled={submitting()}
                            onClick={() => removePlacement(index)}
                          >
                            Remove
                          </button>
                          <Show when={row().kind === "protected"}>
                            <input class={`${ui.input} col-span-4`} value={row().origins}
                              onInput={(event) => updatePlacement(index, { origins: event.currentTarget.value })}
                              placeholder="https://api.example.com" aria-label="Allowed HTTPS origins"
                              autocomplete="off" spellcheck={false} disabled={submitting()} />
                          </Show>
                        </div>
                      )}
                    </Index>
                  </div>
                </Show>
                <div class={ui.actionRow}>
                  <button
                    type="button"
                    class={ui.secondaryButton}
                    disabled={submitting() || secrets.isPending || secretOptions().length === 0}
                    title={secretOptions().length === 0 && !secrets.isPending ? "No active Secrets in this environment" : undefined}
                    onClick={addPlacement}
                  >
                    Add placement
                  </button>
                </div>
              </fieldset>
            </Show>
          </Show>
        </Show>
        <Show when={error()}>
          <p class={ui.fieldError} role="alert">{error()}</p>
        </Show>
        <div class={ui.modalActions}>
          <button type="button" class={ui.secondaryButton} disabled={submitting()} onClick={props.onClose}>
            Cancel
          </button>
          <button type="submit" class={ui.button} disabled={submitting() || !selectedSandbox()}>
            {submitting() ? "Creating..." : "Create"}
          </button>
        </div>
      </form>
    </Modal>
  );
}
