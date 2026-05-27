import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { formatRelative } from "../features/runs/display";
import { ApiError } from "../lib/api";
import {
  createWaitpointPolicy,
  disableWaitpointPolicy,
  listWaitpointPolicies,
  updateWaitpointPolicy,
  type WaitpointPolicy,
  waitpointPolicyRecipients,
} from "../lib/waitpoint-policies";
import { ActionMenu } from "../ui/ActionMenu";
import { Modal } from "../ui/Modal";
import { statusBadgeClass, ui } from "../ui/styles";

const POLICY_NAME_PATTERN = /^[a-z0-9][a-z0-9._-]{0,127}$/;
const EMAIL_PATTERN = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

const POLICY_ERROR_MESSAGES: Record<string, string> = {
  forbidden: "You do not have permission to manage waitpoint policies.",
  invalid_name: "Name must start with a letter or number and contain only letters, numbers, dots, underscores, or dashes.",
  invalid_email: "Enter at least one valid email recipient.",
  duplicate_name: "A policy with this name already exists.",
  not_found: "This policy no longer exists.",
  internal: "Something went wrong. Please try again.",
};

const INTERNAL_ERROR_MESSAGE = "Something went wrong. Please try again.";

function policyErrorMessage(error: unknown): string {
  if (error instanceof ApiError) {
    return POLICY_ERROR_MESSAGES[error.errorKind] ?? error.message ?? INTERNAL_ERROR_MESSAGE;
  }
  return INTERNAL_ERROR_MESSAGE;
}

function parseRecipients(value: string): string[] {
  return value
    .split(/[\n,]+/)
    .map((item) => item.trim())
    .filter(Boolean);
}

function validateName(name: string): string | null {
  if (!POLICY_NAME_PATTERN.test(name.trim())) {
    return POLICY_ERROR_MESSAGES["invalid_name"] ?? INTERNAL_ERROR_MESSAGE;
  }
  return null;
}

function validateRecipients(recipients: string[]): string | null {
  if (recipients.length === 0 || recipients.some((recipient) => !EMAIL_PATTERN.test(recipient))) {
    return POLICY_ERROR_MESSAGES["invalid_email"] ?? INTERNAL_ERROR_MESSAGE;
  }
  return null;
}

function PolicyModal(props: {
  policy: WaitpointPolicy | null;
  onClose: () => void;
  onSaved: () => Promise<void>;
}) {
  const editing = createMemo(() => !!props.policy);
  const [name, setName] = createSignal(props.policy?.name ?? "");
  const [label, setLabel] = createSignal(props.policy?.label ?? "");
  const [recipients, setRecipients] = createSignal(props.policy ? waitpointPolicyRecipients(props.policy).join("\n") : "");
  const [saving, setSaving] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  const save = async (event: Event) => {
    event.preventDefault();
    setError(null);

    const trimmedName = name().trim();
    if (!editing()) {
      const nameError = validateName(trimmedName);
      if (nameError) {
        setError(nameError);
        return;
      }
    }

    const parsedRecipients = parseRecipients(recipients());
    const recipientError = validateRecipients(parsedRecipients);
    if (recipientError) {
      setError(recipientError);
      return;
    }

    const trimmedLabel = label().trim();
    const input = {
      label: trimmedLabel,
      recipients: parsedRecipients,
    };

    setSaving(true);
    try {
      if (editing()) {
        await updateWaitpointPolicy(props.policy!.name, input);
      } else {
        await createWaitpointPolicy({ name: trimmedName, ...input });
      }
      await props.onSaved();
      props.onClose();
    } catch (saveError) {
      setError(policyErrorMessage(saveError));
    } finally {
      setSaving(false);
    }
  };

  return (
    <Modal
      title={editing() ? "Edit waitpoint policy" : "Create waitpoint policy"}
      onClose={props.onClose}
      closeDisabled={saving()}
    >
      <form onSubmit={save}>
        <p class={ui.modalIntro}>
          Policies are referenced by stable name from task code and deliver waitpoint notifications by email.
        </p>
        <label class={ui.field}>
          <span>Name</span>
          <input
            class={ui.input}
            value={name()}
            disabled={editing()}
            autocomplete="off"
            autofocus={!editing()}
            onInput={(event) => setName(event.currentTarget.value)}
          />
        </label>
        <label class={ui.field}>
          <span>Description</span>
          <input
            class={ui.input}
            value={label()}
            autocomplete="off"
            onInput={(event) => setLabel(event.currentTarget.value)}
          />
        </label>
        <label class={ui.field}>
          <span>Email recipients</span>
          <textarea
            class={ui.textarea}
            value={recipients()}
            placeholder={"ops@example.com\nsecurity@example.com"}
            onInput={(event) => setRecipients(event.currentTarget.value)}
          />
        </label>
        <Show when={error()}>
          <p class={ui.error} role="alert">{error()}</p>
        </Show>
        <div class={ui.modalActions}>
          <button type="button" class={ui.secondaryButton} disabled={saving()} onClick={props.onClose}>
            Cancel
          </button>
          <button
            class={ui.button}
            type="submit"
            disabled={saving() || (!editing() && name().trim() === "") || parseRecipients(recipients()).length === 0}
          >
            {saving() ? "Saving..." : "Save"}
          </button>
        </div>
      </form>
    </Modal>
  );
}

function PolicyRow(props: {
  policy: WaitpointPolicy;
  disabling: boolean;
  error: string | null;
  onEdit: (policy: WaitpointPolicy) => void;
  onDisable: (policy: WaitpointPolicy) => void;
}) {
  return (
    <tr>
      <td><code>{props.policy.name}</code></td>
      <td>{props.policy.label || <span class={"text-console-faint"}>—</span>}</td>
      <td>{waitpointPolicyRecipients(props.policy).join(", ")}</td>
      <td><span class={statusBadgeClass("succeeded")}>Active</span></td>
      <td>{formatRelative(props.policy.updated_at)}</td>
      <td>{formatRelative(props.policy.created_at)}</td>
      <td class={ui.actionsCell}>
        <ActionMenu
          label={`Actions for ${props.policy.name}`}
          items={[
            {
              label: "Edit",
              disabled: props.disabling,
              onSelect: () => props.onEdit(props.policy),
            },
            {
              label: "Disable",
              busyLabel: props.disabling ? "Disabling..." : undefined,
              disabled: props.disabling,
              tone: "danger",
              onSelect: () => props.onDisable(props.policy),
            },
          ]}
        />
        <Show when={props.error}>
          <p class={ui.rowError} role="alert">{props.error}</p>
        </Show>
      </td>
    </tr>
  );
}

export function WaitpointPolicies() {
  const queryClient = useQueryClient();
  const [modalPolicy, setModalPolicy] = createSignal<WaitpointPolicy | null | undefined>(undefined);
  const [disablingName, setDisablingName] = createSignal<string | null>(null);
  const [disableError, setDisableError] = createSignal<{ name: string; message: string } | null>(null);
  const policies = createQuery(() => ({
    queryKey: ["waitpoint-policies"],
    queryFn: listWaitpointPolicies,
    retry: false,
  }));

  const invalidatePolicies = () => queryClient.invalidateQueries({ queryKey: ["waitpoint-policies"] });

  const disable = async (policy: WaitpointPolicy) => {
    if (!window.confirm(`Disable waitpoint policy "${policy.name}"?`)) return;
    setDisableError(null);
    setDisablingName(policy.name);
    try {
      await disableWaitpointPolicy(policy.name);
      await invalidatePolicies();
    } catch (error) {
      setDisableError({ name: policy.name, message: policyErrorMessage(error) });
    } finally {
      setDisablingName(null);
    }
  };

  return (
    <>
      <div class={ui.pageHeader}>
        <div>
          <h1 class={ui.h1}>Waitpoint policies</h1>
          <p class={ui.pageSubtitle}>
            Named email delivery policies that task code can reference with a stable policy name.
          </p>
        </div>
        <button class={ui.button} type="button" onClick={() => setModalPolicy(null)}>
          Create policy
        </button>
      </div>

      <Show when={policies.isError}>
        <p class={ui.error} role="alert">{policyErrorMessage(policies.error)}</p>
      </Show>

      <Show when={!policies.isPending} fallback={<p class={ui.muted}>Loading waitpoint policies...</p>}>
        <Show
          when={(policies.data?.policies.length ?? 0) > 0}
          fallback={<p class={ui.emptyState}>No waitpoint policies found.</p>}
        >
          <div class={ui.tableWrap}>
            <table class={ui.dataTable}>
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Description</th>
                  <th>Email recipients</th>
                  <th>Status</th>
                  <th>Updated</th>
                  <th>Created</th>
                  <th><span class="sr-only">Actions</span></th>
                </tr>
              </thead>
              <tbody>
                <For each={policies.data?.policies ?? []}>
                  {(policy) => (
                    <PolicyRow
                      policy={policy}
                      disabling={disablingName() === policy.name}
                      error={disableError()?.name === policy.name ? disableError()?.message ?? null : null}
                      onEdit={(selected) => setModalPolicy(selected)}
                      onDisable={disable}
                    />
                  )}
                </For>
              </tbody>
            </table>
          </div>
        </Show>
      </Show>

      <Show when={modalPolicy() !== undefined}>
        <PolicyModal
          policy={modalPolicy() ?? null}
          onClose={() => setModalPolicy(undefined)}
          onSaved={invalidatePolicies}
        />
      </Show>
    </>
  );
}
