import { EnvironmentAccessSetup } from "../access/EnvironmentAccessSetup";
import { A } from "@solidjs/router";
import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createEffect, createMemo, createSignal, For, Match, Show, Switch } from "solid-js";
import { errorMessage } from "../../lib/error";
import { usePermission } from "../../lib/me";
import { createSecret, listSecrets, rotateSecret } from "../../lib/secrets";
import { CodeBlock } from "../../ui/CodeBlock";
import { Field, FormActions, Segmented, SecretInput } from "../../ui/Field";
import { Notice } from "../../ui/Notice";
import { Panel } from "../../ui/Panel";
import { isDenied } from "../../ui/QueryState";
import { Loading, SkeletonBar } from "../../ui/Skeleton";
import { Steps, type Step } from "../../ui/Steps";
import { cx, ui } from "../../ui/styles";
import { HARNESSES, PROVIDERS, setupPrompt, type HarnessID, type ProviderID } from "./harnesses";

const SAVE_ERRORS: Record<string, string> = { forbidden: "You do not have permission to manage secrets." };

const harnessCard =
  "flex h-11 cursor-pointer items-center gap-2.5 border border-console-border bg-console-bg-panel px-3 transition hover:border-console-border-strong hover:bg-console-surface has-checked:border-2 has-checked:border-console-accent has-checked:bg-console-accent-soft has-checked:px-[11px] has-focus-visible:ring-2 has-focus-visible:ring-console-focus";

function HarnessPicker(props: { value: HarnessID; onChange: (value: HarnessID) => void }) {
  return (
    <fieldset class="m-0 border-0 p-0">
      <legend class="sr-only">Coding agent</legend>
      <div class="grid grid-cols-4 gap-2 max-md:grid-cols-2">
        <For each={HARNESSES}>
          {(harness) => (
            <label class={harnessCard}>
              <input class="sr-only" type="radio" name="setup-harness" value={harness.id} checked={props.value === harness.id} onChange={() => props.onChange(harness.id)} />
              <img src={harness.logo} alt="" class="size-5 shrink-0" />
              <span class="truncate font-mono text-chrome font-medium text-console-text">{harness.label}</span>
            </label>
          )}
        </For>
      </div>
    </fieldset>
  );
}

// The provider's key as a Secret in this Environment: add it, replace it, or
// say who can.
function ModelKey(props: { projectID: string; environmentID: string; provider: ProviderID; otherChoices: boolean; onReady: (ready: boolean) => void }) {
  const queryClient = useQueryClient();
  const permission = usePermission();
  const provider = () => PROVIDERS[props.provider];
  const secrets = createQuery(() => ({
    queryKey: ["secrets", props.projectID, props.environmentID],
    queryFn: () => listSecrets(props.projectID, props.environmentID),
    retry: false,
  }));
  const secret = () => secrets.data?.secrets.find((item) => item.secret === provider().secret);
  const canWrite = () => permission.has("secrets.write", { projectID: props.projectID, environmentID: props.environmentID });
  const [replacing, setReplacing] = createSignal(false);
  const [value, setValue] = createSignal("");
  const [saving, setSaving] = createSignal(false);
  const [saved, setSaved] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);
  const [announcement, setAnnouncement] = createSignal("");
  let replaceButton: HTMLButtonElement | undefined;
  const secretsHref = () => `/settings/secrets?project=${encodeURIComponent(props.projectID)}&environment=${encodeURIComponent(props.environmentID)}`;

  const save = async (event: Event) => {
    event.preventDefault();
    setError(null);
    setSaving(true);
    try {
      if (secret()) await rotateSecret(provider().secret, value(), props.projectID, props.environmentID);
      else await createSecret(provider().secret, value(), props.projectID, props.environmentID);
      setValue("");
      setReplacing(false);
      setSaved(true);
      setAnnouncement(`${provider().secret} saved`);
      await queryClient.invalidateQueries({ queryKey: ["secrets", props.projectID, props.environmentID] });
      // The form is gone; keep keyboard focus on the key's row.
      replaceButton?.focus();
    } catch (saveError) {
      setError(errorMessage(saveError, "Could not save the key. Please try again.", SAVE_ERRORS));
    } finally {
      setSaving(false);
    }
  };

  const form = () => (
    <form class="grid gap-2" onSubmit={(event) => void save(event)}>
      <Field label={provider().secret} hint={<>Get a key at <a class={ui.monoLink} href={provider().keyURL} target="_blank" rel="noreferrer">{new URL(provider().keyURL).host}</a>.</>} error={error()} flush>
        <SecretInput value={value()} placeholder="Paste the key" onInput={(event) => setValue(event.currentTarget.value)} />
      </Field>
      <Show when={replacing()} fallback={
        <div class={ui.modalActions}>
          <button type="submit" class={ui.button} disabled={saving() || value().trim() === ""}>{saving() ? "Saving…" : "Save key"}</button>
        </div>
      }>
        <FormActions submitLabel="Save key" busyLabel="Saving…" busy={saving()} disabled={value().trim() === ""} onCancel={() => { setReplacing(false); setValue(""); setError(null); }} />
      </Show>
    </form>
  );

  const state = createMemo(() => {
    if (secrets.isPending) return "loading";
    if (secrets.isError) return isDenied(secrets.error) ? "denied" : "error";
    return secret()?.status ?? "missing";
  });
  // Someone who cannot add the key can still hand setup to their agent; a
  // secret manager adds the key before it deploys.
  const ready = () => state() === "active" || ((state() === "missing" || state() === "denied") && !canWrite());
  createEffect(() => props.onReady(ready()));

  return (
    <div class="grid gap-2">
      <span>Your agent's code sends this key only to <code class="font-mono text-chrome text-console-text">{provider().origin}</code>; the computer sees a placeholder, and your coding agent never sees the key.</span>
      <Switch>
        <Match when={state() === "loading"}>
          <Loading label="Checking secrets"><SkeletonBar width="10rem" /></Loading>
        </Match>
        <Match when={state() === "active" && !replacing()}>
          <div class="flex flex-wrap items-center gap-2">
            <span class="font-mono text-chrome text-console-success">{saved() ? "Saved" : "Set"}</span>
            <span><code class="font-mono text-chrome text-console-text">{provider().secret}</code> is set in this environment.</span>
            <Show when={canWrite()}>
              <button ref={replaceButton} type="button" class={ui.compactButton} onClick={() => { setSaved(false); setAnnouncement(""); setReplacing(true); }}>Replace key</button>
            </Show>
          </div>
        </Match>
        <Match when={state() === "revoked"}>
          <Notice tone="warning">
            {provider().secret} was revoked in this environment, and a revoked ID cannot be used again. {props.otherChoices ? "Choose another agent or provider" : "Choose another agent"}, or ask a member with Manage secrets in this environment how to proceed.
          </Notice>
        </Match>
        <Match when={state() === "error"}>
          <Notice tone="danger" action={<button type="button" class={ui.monoLink} onClick={() => void secrets.refetch()}>Retry</button>}>Could not check this environment's secrets.</Notice>
        </Match>
        <Match when={!canWrite()}>
          <Notice tone="info" action={<A href={secretsHref()}>Secrets</A>}>
            A member with Manage secrets adds <code class="font-mono">{provider().secret}</code> to this environment. You can copy the prompt now; your agent asks you to confirm the key before it deploys.
          </Notice>
        </Match>
        <Match when={true}>{form()}</Match>
      </Switch>
      <span class="sr-only" aria-live="polite">{announcement()}</span>
    </div>
  );
}

// Overview's first-run panel: pick the coding agent to run on Helmr and its
// model provider, add the provider key, then copy a prompt that hands the rest
// of setup to the coding agent the person already works with.
export function SetUpPanel(props: { projectID: string; environmentID: string }) {
  const [harnessID, setHarnessID] = createSignal<HarnessID>("claude-code");
  const [chosen, setChosen] = createSignal<ProviderID>("anthropic");
  const [ready, setReady] = createSignal(false);
  const harness = () => HARNESSES.find((item) => item.id === harnessID())!;
  const binding = () => harness().bindings.find((item) => item.provider === chosen()) ?? harness().bindings[0]!;
  const provider = () => PROVIDERS[binding().provider];
  const prompt = () => setupPrompt({ origin: window.location.origin, harness: harnessID(), provider: provider().id, projectID: props.projectID, environmentID: props.environmentID });
  const pick = (id: HarnessID) => {
    setHarnessID(id);
    setChosen(HARNESSES.find((item) => item.id === id)!.bindings[0]!.provider);
  };

  const harnessStep = <HarnessPicker value={harnessID()} onChange={pick} />;
  const providerStep = (
    <>
      <Show when={harness().bindings.length > 1} fallback={<span>{harness().label} uses {provider().label}.</span>}>
        <div><Segmented label="Model provider" value={provider().id} options={harness().bindings.map((item) => ({ value: item.provider, label: PROVIDERS[item.provider].label }))} onChange={setChosen} /></div>
      </Show>
      <span>Uses <code class="font-mono text-chrome text-console-text">{binding().model}</code>; change it later in <code class="font-mono text-chrome">agents/settings.ts</code>.</span>
    </>
  );
  // Keyed by scope and provider, so a half-typed key never carries over to
  // another Environment's or provider's Secret.
  const keyStep = (
    <Show when={`${props.projectID}/${props.environmentID}/${provider().id}`} keyed>
      {(_scope) => <ModelKey projectID={props.projectID} environmentID={props.environmentID} provider={provider().id} otherChoices={harness().bindings.length > 1} onReady={setReady} />}
    </Show>
  );
  const promptStep = (
    <>
      <span>It signs in, creates and deploys the agent, and tells you when to approve the sign-in and connect Slack.</span>
      <CodeBlock code={prompt()} copy={ready()} label="Setup prompt" class={cx(!ready() && "opacity-60")} />
      <Show when={!ready()}>
        <span>You can copy the prompt once the key is set.</span>
      </Show>
    </>
  );

  // One stable list whose states follow the key: rebuilding it would replace
  // every step's row and drop keyboard focus.
  const steps: Step[] = [
    { title: "Which agent do you want to run on Helmr?", state: "done", description: harnessStep },
    { title: "Which model provider?", state: "done", description: providerStep },
    { title: "Add the key it needs", get state() { return ready() ? "done" : "current"; }, description: keyStep },
    { title: "Paste this into the coding agent you work with", get state() { return ready() ? "current" : "upcoming"; }, description: promptStep },
  ];

  return (
    <Panel title="Set up">
      <EnvironmentAccessSetup projectID={props.projectID} environmentID={props.environmentID} />
      <Steps label="Set up" steps={steps} />
    </Panel>
  );
}
