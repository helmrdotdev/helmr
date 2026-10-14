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

// A color picker with preset swatches.
export function ColorField(props: {
  value: string;
  onChange: (colorHex: string) => void;
  presets: readonly string[];
  label?: string;
  // Names the picker for assistive technology, e.g. "Environment color".
  ariaLabel?: string;
}) {
  const label = () => props.label ?? "Color";
  return (
    <div class="mb-3 grid gap-1">
      <span class={ui.fieldLabel}>{label()}</span>
      <div class="flex items-center gap-2">
        <input
          type="color"
          class="size-8 cursor-pointer border border-console-border bg-console-surface p-0.5"
          value={props.value}
          onInput={(event) => props.onChange(event.currentTarget.value.trim().toUpperCase())}
          aria-label={props.ariaLabel ?? label()}
        />
        <div class="flex flex-wrap gap-1.5">
          <For each={props.presets}>
            {(preset) => (
              <button
                type="button"
                class="size-6 cursor-pointer border border-console-border bg-console-surface p-0.5 aria-pressed:border-console-text"
                aria-label={`Use ${preset}`}
                aria-pressed={preset.toUpperCase() === props.value.toUpperCase()}
                onClick={() => props.onChange(preset)}
              >
                <span class="block size-full" style={{ "background-color": preset }} />
              </button>
            )}
          </For>
        </div>
      </div>
    </div>
  );
}

// Two to five mutually exclusive options that change a view.
export function Segmented<T extends string>(props: {
  label: string;
  value: T;
  options: readonly { value: T; label: string }[];
  onChange: (value: T) => void;
  disabled?: boolean;
}) {
  return (
    <div class="inline-flex overflow-hidden rounded-xs border border-console-border-strong" role="group" aria-label={props.label}>
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
          </button>
        )}
      </For>
    </div>
  );
}

type Choice<T extends string> = { value: T; label: string; description?: string };

// Options that need a description: one choice (radio) or several (checkbox).
export function ChoiceCards<T extends string>(
  props: { label: string; name: string; options: readonly Choice<T>[]; disabled?: boolean } & (
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
  return (
    <fieldset class={ui.fieldSet}>
      <legend class={ui.fieldLegend}>{props.label}</legend>
      <div class="grid gap-1.5">
        <For each={props.options}>
          {(option) => (
            <label class={ui.permissionOption}>
              <input
                type={props.multiple ? "checkbox" : "radio"}
                name={props.name}
                value={option.value}
                checked={checked(option.value)}
                disabled={props.disabled}
                onChange={(event) => toggle(option.value, event.currentTarget.checked)}
              />
              <span>
                <strong>{option.label}</strong>
                <Show when={option.description}>
                  <span>{option.description}</span>
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
  groups: readonly { label: string; options: readonly { value: T; label: string }[] }[];
  value: readonly T[];
  onChange: (value: T[]) => void;
  disabled?: boolean;
}) {
  const set = createMemo(() => new Set(props.value));
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
                  <span>{group.label}</span>
                  <label class="flex cursor-pointer items-center gap-1.5 font-sans text-chrome normal-case tracking-normal text-console-muted">
                    <input
                      type="checkbox"
                      class="accent-console-accent"
                      checked={selected() === values().length}
                      ref={(element) => createEffect(() => (element.indeterminate = selected() > 0 && selected() < values().length))}
                      disabled={props.disabled}
                      aria-label={`Select all ${group.label}`}
                      onChange={(event) => update(values(), event.currentTarget.checked)}
                    />
                    Select all
                  </label>
                </div>
                <div class="flex flex-wrap gap-x-4 gap-y-1.5 px-2.5 py-2">
                  <For each={group.options}>
                    {(option) => (
                      <label class="flex cursor-pointer items-center gap-1.5 text-control text-console-text">
                        <input
                          type="checkbox"
                          class="accent-console-accent"
                          checked={set().has(option.value)}
                          disabled={props.disabled}
                          onChange={(event) => update([option.value], event.currentTarget.checked)}
                        />
                        {option.label}
                      </label>
                    )}
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
  disabled?: boolean | undefined;
  tone?: "default" | "danger" | undefined;
  // Without a form around the actions, the primary action calls this instead of submitting.
  onSubmit?: (() => void) | undefined;
}) {
  return (
    <div class={ui.modalActions}>
      <button type="button" class={ui.secondaryButton} disabled={props.busy} onClick={() => props.onCancel()}>
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
