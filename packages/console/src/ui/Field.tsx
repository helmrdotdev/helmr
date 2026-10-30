import { createContext, createEffect, createMemo, createSignal, createUniqueId, For, Show, splitProps, useContext, type JSX } from "solid-js";
import { slugify } from "../lib/slug";
import { cx, ui } from "./styles";

type FieldControl = {
  id: string;
  labelID: string;
  describedBy: () => string | undefined;
  invalid: () => boolean;
};

const FieldContext = createContext<FieldControl>();

// The id, label, hint and error wiring of the surrounding Field, for controls.
export function useFieldControl(): FieldControl | undefined {
  return useContext(FieldContext);
}

// A labelled form row: label, one control, an optional hint and an error that is
// announced when it appears. Controls inside pick up the label, hint and error.
export function Field(props: {
  label: string;
  hint?: JSX.Element | undefined;
  error?: string | null | undefined;
  // Drops the 12px gap below the field, for a field alone in a row.
  flush?: boolean | undefined;
  class?: string | undefined;
  children: JSX.Element;
}) {
  const id = createUniqueId();
  const hintID = `${id}-hint`;
  const errorID = `${id}-error`;
  const control: FieldControl = {
    id,
    labelID: `${id}-label`,
    describedBy: () => [props.hint ? hintID : "", props.error ? errorID : ""].filter(Boolean).join(" ") || undefined,
    invalid: () => !!props.error,
  };
  return (
    <div class={cx("grid gap-1", !props.flush && "mb-3", props.class)}>
      <label id={control.labelID} for={id} class={ui.fieldLabel}>{props.label}</label>
      <FieldContext.Provider value={control}>{props.children}</FieldContext.Provider>
      <Show when={props.hint}>
        <p id={hintID} class={cx("m-0", ui.fieldHint)}>{props.hint}</p>
      </Show>
      <Show when={props.error}>
        <p id={errorID} class="m-0 text-control font-medium text-console-danger" role="alert">{props.error}</p>
      </Show>
    </div>
  );
}

type InputProps = Omit<JSX.InputHTMLAttributes<HTMLInputElement>, "class" | "id" | "type"> & { class?: string };

function fieldAttributes(field: FieldControl | undefined) {
  return {
    id: field?.id,
    "aria-describedby": field?.describedBy(),
    "aria-invalid": field?.invalid() || undefined,
  };
}

export function TextInput(props: InputProps & { type?: "text" | "email" | "url" | "number" }) {
  const field = useFieldControl();
  const [local, rest] = splitProps(props, ["class", "type"]);
  return <input {...rest} {...fieldAttributes(field)} type={local.type ?? "text"} class={cx(ui.input, local.class)} />;
}

export function TextArea(props: Omit<JSX.TextareaHTMLAttributes<HTMLTextAreaElement>, "class" | "id"> & { class?: string }) {
  const field = useFieldControl();
  const [local, rest] = splitProps(props, ["class"]);
  return <textarea {...rest} {...fieldAttributes(field)} class={cx(ui.textarea, local.class)} />;
}

// A write-only value: it is typed once and never shown again.
export function SecretInput(props: InputProps) {
  const field = useFieldControl();
  const [local, rest] = splitProps(props, ["class"]);
  return (
    <input
      {...rest}
      {...fieldAttributes(field)}
      type="password"
      autocomplete="new-password"
      spellcheck={false}
      class={cx(ui.input, local.class)}
    />
  );
}

// A write-only value that may span lines (PEM keys, multi-line tokens): line
// breaks are kept, it is never prefilled or shown after saving, and it is masked
// while entered where the browser supports text masking.
export function SecretTextArea(props: Omit<JSX.TextareaHTMLAttributes<HTMLTextAreaElement>, "class" | "id" | "autocomplete" | "spellcheck"> & { class?: string }) {
  const field = useFieldControl();
  const [local, rest] = splitProps(props, ["class"]);
  return (
    <textarea
      {...rest}
      {...fieldAttributes(field)}
      autocomplete="off"
      autocorrect="off"
      autocapitalize="off"
      spellcheck={false}
      wrap="off"
      class={cx(ui.textarea, "font-mono text-chrome [-webkit-text-security:disc]", local.class)}
    />
  );
}

export function SearchInput(props: InputProps & { "aria-label": string }) {
  const [local, rest] = splitProps(props, ["class"]);
  return <input {...rest} type="search" autocomplete="off" spellcheck={false} class={cx(ui.input, local.class)} />;
}

export type DerivedSlug = {
  value: () => string;
  edit: (value: string) => void;
  edited: () => boolean;
  reset: () => void;
};

// An identifier that follows a display name until the person edits it.
export function createDerivedSlug(source: () => string): DerivedSlug {
  const [manual, setManual] = createSignal<string | null>(null);
  return {
    value: () => manual() ?? slugify(source()),
    edit: setManual,
    edited: () => manual() !== null,
    reset: () => setManual(null),
  };
}

export function SlugInput(props: Omit<InputProps, "value" | "onInput"> & { slug: DerivedSlug }) {
  const field = useFieldControl();
  const [local, rest] = splitProps(props, ["class", "slug"]);
  return (
    <input
      {...rest}
      {...fieldAttributes(field)}
      type="text"
      autocomplete="off"
      spellcheck={false}
      value={local.slug.value()}
      onInput={(event) => local.slug.edit(event.currentTarget.value)}
      class={cx(ui.monoInput, local.class)}
    />
  );
}

// A color picker: preset swatches, then one custom swatch that opens the
// browser's color picker. Every swatch is 24px; the chosen one has the accent
// ring. The custom swatch shows its color only when it is not a preset, so a
// color never appears twice.
export function ColorField(props: {
  value: string;
  onChange: (colorHex: string) => void;
  presets: readonly string[];
  label?: string;
  // Names the picker for assistive technology, e.g. "Environment color".
  ariaLabel?: string;
}) {
  const label = () => props.label ?? "Color";
  const custom = () => !props.presets.some((preset) => preset.toUpperCase() === props.value.toUpperCase());
  return (
    <div class="mb-3 grid gap-1">
      <span class={ui.fieldLabel}>{label()}</span>
      <div class="flex flex-wrap items-center gap-1.5">
        <For each={props.presets}>
          {(preset) => (
            <button
              type="button"
              class={SWATCH}
              aria-label={`Use ${preset}`}
              aria-pressed={preset.toUpperCase() === props.value.toUpperCase()}
              onClick={() => props.onChange(preset)}
            >
              <span class="block size-full" style={{ "background-color": preset }} />
            </button>
          )}
        </For>
        <label class={cx(SWATCH, "relative has-focus-visible:outline-2 has-focus-visible:outline-offset-1 has-focus-visible:outline-solid")} data-selected={custom() ? "true" : undefined} title="Custom color">
          <input
            type="color"
            class="absolute inset-0 size-full cursor-pointer opacity-0"
            value={props.value}
            onInput={(event) => props.onChange(event.currentTarget.value.trim().toUpperCase())}
            aria-label={props.ariaLabel ?? label()}
          />
          <Show when={custom()} fallback={<span class="grid size-full place-items-center font-mono text-control leading-none text-console-muted" aria-hidden="true">+</span>}>
            <span class="block size-full" style={{ "background-color": props.value }} />
          </Show>
        </label>
      </div>
    </div>
  );
}

const SWATCH =
  "grid size-6 shrink-0 cursor-pointer place-items-center rounded-xs border border-console-border bg-console-surface p-0.5 aria-pressed:border-2 aria-pressed:border-console-accent aria-pressed:p-px data-[selected=true]:border-2 data-[selected=true]:border-console-accent data-[selected=true]:p-px";

// Two to five mutually exclusive options that change a view. An option's
// `count` follows its label, as in a filter showing how many items each shows.
export function Segmented<T extends string>(props: {
  label: string;
  value: T;
  options: readonly { value: T; label: string; count?: number }[];
  onChange: (value: T) => void;
  disabled?: boolean;
}) {
  return (
    <div class="inline-flex w-fit max-w-full flex-wrap justify-self-start overflow-hidden rounded-xs border border-console-border-strong" role="group" aria-label={props.label}>
      <For each={props.options}>
        {(option) => (
          <button
            type="button"
            class={ui.segment}
            aria-pressed={option.value === props.value}
            disabled={props.disabled}
            onClick={() => props.onChange(option.value)}
          >
            {option.label}
            <Show when={option.count !== undefined}>
              {" "}<span class="ml-0.5 text-console-subtle">{option.count}</span>
            </Show>
          </button>
        )}
      </For>
    </div>
  );
}

type Choice<T extends string> = { value: T; label: string; description?: string };

// Options that need a description: one choice (radio) or several (checkbox).
export function ChoiceCards<T extends string>(
  // `hideLabel` keeps the legend for assistive technology only, where the
  // container's title already names the group.
  props: { label: string; name: string; options: readonly Choice<T>[]; disabled?: boolean; hideLabel?: boolean } & (
    | { multiple?: false; value: T; onChange: (value: T) => void }
    | { multiple: true; value: readonly T[]; onChange: (value: T[]) => void }
  ),
) {
  const checked = (value: T) => (props.multiple ? props.value.includes(value) : props.value === value);
  const toggle = (value: T, on: boolean) => {
    if (props.multiple) {
      const next = props.value.filter((item) => item !== value);
      props.onChange(on ? [...next, value] : next);
    } else if (on) {
      props.onChange(value);
    }
  };
  const id = createUniqueId();
  return (
    <fieldset class={ui.fieldSet}>
      <legend class={props.hideLabel ? "sr-only" : ui.fieldLegend}>{props.label}</legend>
      <div class="grid gap-1.5">
        <For each={props.options}>
          {(option, index) => (
            <label class={ui.choiceCard}>
              <input
                type={props.multiple ? "checkbox" : "radio"}
                name={props.name}
                value={option.value}
                checked={checked(option.value)}
                disabled={props.disabled}
                aria-label={option.label}
                aria-describedby={option.description ? `${id}-${index()}` : undefined}
                onChange={(event) => toggle(option.value, event.currentTarget.checked)}
              />
              <span>
                <strong>{option.label}</strong>
                <Show when={option.description}>
                  <span id={`${id}-${index()}`}>{option.description}</span>
                </Show>
              </span>
            </label>
          )}
        </For>
      </div>
    </fieldset>
  );
}

// Many independent toggles, grouped under headings with "Select all" per group.
export function CheckboxGroup<T extends string>(props: {
  label: string;
  groups: readonly { label: string; options: readonly { value: T; label: string; description?: string }[] }[];
  value: readonly T[];
  onChange: (value: T[]) => void;
  disabled?: boolean;
}) {
  const set = createMemo(() => new Set(props.value));
  const id = createUniqueId();
  const update = (values: readonly T[], on: boolean) => {
    const next = props.value.filter((value) => !values.includes(value));
    props.onChange(on ? [...next, ...values] : next);
  };
  return (
    <fieldset class={ui.fieldSet}>
      <legend class={ui.fieldLegend}>{props.label}</legend>
      <div class="grid gap-2">
        <For each={props.groups}>
          {(group) => {
            const values = () => group.options.map((option) => option.value);
            const selected = () => values().filter((value) => set().has(value)).length;
            return (
              <div class="border border-console-border">
                <div class={cx("flex items-center justify-between gap-2 border-b border-console-border-soft bg-console-bg-panel px-2.5 py-1.5", ui.microLabel)}>
                  <span>{group.label} <span class="normal-case tracking-normal text-console-muted">· {selected()} of {values().length}</span></span>
                  <label class="flex cursor-pointer items-center gap-1.5 font-sans text-chrome normal-case tracking-normal text-console-muted">
                    <input
                      type="checkbox"
                      class="accent-console-accent"
                      checked={selected() === values().length}
                      ref={(element) => createEffect(() => (element.indeterminate = selected() > 0 && selected() < values().length))}
                      disabled={props.disabled}
                      aria-label={`Select all ${group.label.toLowerCase()}`}
                      onChange={(event) => update(values(), event.currentTarget.checked)}
                    />
                    Select all
                  </label>
                </div>
                <div class={group.options.some((option) => option.description) ? "grid gap-2 px-2.5 py-2" : "flex flex-wrap gap-x-4 gap-y-1.5 px-2.5 py-2"}>
                  <For each={group.options}>
                    {(option) => {
                      const hintID = `${id}-${option.value}`;
                      return (
                        <div class="grid gap-0.5">
                          <label class="flex cursor-pointer items-center gap-1.5 text-control text-console-text">
                            <input
                              type="checkbox"
                              class="accent-console-accent"
                              checked={set().has(option.value)}
                              disabled={props.disabled}
                              aria-describedby={option.description ? hintID : undefined}
                              onChange={(event) => update([option.value], event.currentTarget.checked)}
                            />
                            {option.label}
                          </label>
                          <Show when={option.description}>
                            <span id={hintID} class={cx("block pl-5", ui.fieldHint)}>{option.description}</span>
                          </Show>
                        </div>
                      );
                    }}
                  </For>
                </div>
              </div>
            );
          }}
        </For>
      </div>
    </fieldset>
  );
}

// Cancel, then the primary action, right-aligned. While busy, Cancel is disabled
// and the primary action shows its busy label.
export function FormActions(props: {
  submitLabel: string;
  busyLabel: string;
  busy: boolean;
  onCancel: () => void;
  cancelLabel?: string | undefined;
  // Cancel has work of its own in flight (e.g. signing out).
  cancelDisabled?: boolean | undefined;
  disabled?: boolean | undefined;
  tone?: "default" | "danger" | undefined;
  // Without a form around the actions, the primary action calls this instead of submitting.
  onSubmit?: (() => void) | undefined;
  // Replaces the default spacing, e.g. a dialog's pinned footer.
  class?: string | undefined;
}) {
  return (
    <div class={props.class ?? ui.modalActions}>
      <button type="button" class={ui.secondaryButton} disabled={props.busy || props.cancelDisabled} onClick={() => props.onCancel()}>
        {props.cancelLabel ?? "Cancel"}
      </button>
      <button
        type={props.onSubmit ? "button" : "submit"}
        class={props.tone === "danger" ? ui.dangerOutlineButton : ui.button}
        disabled={props.busy || props.disabled}
        onClick={() => props.onSubmit?.()}
      >
        {props.busy ? props.busyLabel : props.submitLabel}
      </button>
    </div>
  );
}
