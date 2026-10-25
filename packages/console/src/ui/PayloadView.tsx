import { createMemo, createSignal, For, Show, type JSX } from "solid-js";
import { Icon } from "./Icon";
import type { JsonValue } from "../../../../sdk/typescript/src/contract";
import { byteLength, childPage, formatBytes, jsonPath, pathMatches, preview, scalar, stringClamp, textLineClamp, treeLimit } from "./payload";
import { cx, ui } from "./styles";
import { useClipboard } from "./useClipboard";

type Copy = (text: string) => void;

// A framed payload with a 24px header: what it is on the left, actions on the
// right. Inline (a log line's open record) it has no frame: a thin strip of
// actions sits directly under the row that opened it, and the value follows.
function PayloadFrame(props: { label: string; meta: string; actions: JSX.Element; children: JSX.Element; class?: string | undefined; inline?: boolean | undefined }) {
  return (
    <Show when={!props.inline} fallback={
      <figure class={cx("m-0 min-w-0", props.class)} aria-label={props.label}>
        <figcaption class="flex h-6 items-center gap-3.5 font-mono text-micro text-console-subtle">
          {props.actions}
          <span class="ml-auto min-w-0 truncate">{props.meta}</span>
        </figcaption>
        {props.children}
      </figure>
    }>
      <figure class={cx("m-0 min-w-0 border border-console-border bg-console-surface", props.class)} aria-label={props.label}>
        <figcaption class="flex h-6 items-center gap-2 border-b border-console-border-soft bg-console-bg-panel px-2 font-mono text-micro text-console-subtle">
          <span class="min-w-0 truncate">{props.meta}</span>
          <span class="ml-auto flex shrink-0 items-center gap-3">{props.actions}</span>
        </figcaption>
        {props.children}
      </figure>
    </Show>
  );
}

function CopyAction(props: { text: () => string; label: string }) {
  const clipboard = useClipboard();
  return (
    <>
      <button type="button" class={ui.payloadAction} onClick={() => void clipboard.copy(props.text())}>
        {clipboard.state() === "copied" ? "Copied" : clipboard.state() === "failed" ? "Could not copy" : props.label}
      </button>
      <clipboard.LiveRegion />
    </>
  );
}

// Any JSON value as a folding tree, or as raw JSON. Children appear in pages of
// 100, strings are clamped at 1 KB, and values over 64 KB show raw text only.
// `open` lists node paths (keys joined by ".") that start open; `pinned` lists
// the paths a summary line uses, which the tree marks. `actions` adds controls
// to the strip, and `kind` names the value in it ("json" by default).
export function JsonView(props: {
  value: JsonValue;
  label: string;
  class?: string | undefined;
  inline?: boolean | undefined;
  open?: ReadonlySet<string> | undefined;
  pinned?: ReadonlySet<string> | undefined;
  actions?: JSX.Element;
  kind?: string | undefined;
}) {
  const raw = createMemo(() => JSON.stringify(props.value, null, 2));
  const size = createMemo(() => byteLength(raw()));
  const tooLarge = () => size() > treeLimit;
  // A scalar has nothing to fold: it shows as raw JSON without the toggle.
  const scalarRoot = () => props.value === null || typeof props.value !== "object";
  const [mode, setMode] = createSignal<"tree" | "raw">("tree");
  const clipboard = useClipboard();
  const copy: Copy = text => void clipboard.copy(text);
  const tab = (value: "tree" | "raw", label: string) => (
    <button type="button" aria-pressed={mode() === value} class={cx(ui.payloadAction, "text-console-subtle aria-pressed:text-console-text aria-pressed:underline aria-pressed:underline-offset-4")} onClick={() => setMode(value)}>{label}</button>
  );
  return (
    <PayloadFrame
      label={props.label}
      class={props.class}
      inline={props.inline}
      meta={`${props.kind ?? "json"} · ${formatBytes(size())}`}
      actions={<>
        <Show when={!tooLarge() && !scalarRoot()}>{tab("tree", "Tree")}{tab("raw", "Raw")}</Show>
        <CopyAction label="Copy JSON" text={raw} />
        {props.actions}
      </>}
    >
      <clipboard.LiveRegion />
      <Show when={!tooLarge()} fallback={
        <div class={props.inline ? "py-1" : "px-2 py-1"}>
          <pre class={ui.payloadRaw}>{raw().slice(0, treeLimit)}</pre>
          <p class="m-0 text-body text-console-muted">Over 64 KB the tree is skipped. Showing the first 64 KB; Copy JSON copies all of it.</p>
        </div>
      }>
        <Show when={mode() === "tree" && !scalarRoot()} fallback={<pre class={cx(ui.payloadRaw, props.inline ? "py-1" : "px-2 py-1")}>{raw()}</pre>}>
          <div class={cx("py-1 font-mono text-control leading-5", props.inline && "-ml-2")}>
            <JsonNode value={props.value} path={[]} depth={0} open copy={copy} defaults={props.open} pinned={props.pinned} />
          </div>
        </Show>
      </Show>
    </PayloadFrame>
  );
}

function JsonNode(props: {
  name?: string | number;
  value: JsonValue;
  path: (string | number)[];
  depth: number;
  open?: boolean;
  copy: Copy;
  defaults?: ReadonlySet<string> | undefined;
  pinned?: ReadonlySet<string> | undefined;
}) {
  const key = () => props.path.join(".");
  const [open, setOpen] = createSignal(!!props.open || pathMatches(props.defaults, props.path));
  const pinned = () => !!props.pinned?.has(key());
  const [shown, setShown] = createSignal(childPage);
  const branch = () => props.value !== null && typeof props.value === "object";
  const entries = createMemo((): [string | number, JsonValue][] => {
    const value = props.value;
    if (Array.isArray(value)) return value.map((item, index) => [index, item]);
    if (value !== null && typeof value === "object") return Object.entries(value) as [string, JsonValue][];
    return [];
  });
  const brackets = () => (Array.isArray(props.value) ? ["[", "]"] : ["{", "}"]);
  const count = () => { const n = entries().length; return Array.isArray(props.value) ? `${n} ${n === 1 ? "item" : "items"}` : `${n} ${n === 1 ? "key" : "keys"}`; };
  return (
    <div>
      {/* A branch's toggle is its one tab stop; Copy actions show on hover or focus.
          Scalars copy through their parent's Copy value or the header's Copy JSON. */}
      <div class="group flex min-h-5 items-start gap-1 px-2 hover:bg-console-accent-soft focus-within:bg-console-accent-soft" style={{ "padding-left": `${8 + props.depth * 16}px` }}>
        <Show when={branch()} fallback={<span class="w-3 shrink-0" />}>
          <button type="button" class="grid h-5 w-3 shrink-0 cursor-pointer place-items-center border-0 bg-transparent p-0 text-console-subtle" aria-expanded={open()} aria-label={`${open() ? "Collapse" : "Expand"} ${props.name ?? "value"}`} onClick={() => setOpen(!open())}><Icon name={open() ? "chevron-down" : "chevron-right"} class="size-2.5" /></button>
        </Show>
        <span class="min-w-0 flex-1 break-words">
          <span class={cx(pinned() && ui.payloadPinned)}>
          <Show when={props.name !== undefined}>
            <span class={typeof props.name === "number" ? "text-console-subtle" : "text-console-syntax-key"}>{props.name}</span>
            <span class="text-console-subtle">: </span>
          </Show>
          <Show when={branch()} fallback={<Scalar value={props.value} copy={props.copy} />}>
            <Show when={open()} fallback={<button type="button" tabindex="-1" class="cursor-pointer border-0 bg-transparent p-0 text-left font-mono text-control text-console-subtle" onClick={() => setOpen(true)}>{preview(props.value)}</button>}>
              <span class="text-console-subtle">{brackets()[0]} <span class="text-micro">{count()}</span></span>
            </Show>
          </Show>
          </span>
        </span>
        <span class="hidden shrink-0 gap-2 group-hover:flex group-focus-within:flex">
          <button type="button" class={ui.payloadAction} onClick={() => props.copy(typeof props.value === "string" ? props.value : JSON.stringify(props.value, null, 2))}>Copy value</button>
          <button type="button" class={ui.payloadAction} onClick={() => props.copy(jsonPath(props.path))}>Copy path</button>
        </span>
      </div>
      <Show when={branch() && open()}>
        <div>
          <For each={entries().slice(0, shown())}>
            {([name, value]) => <JsonNode name={name} value={value} path={[...props.path, name]} depth={props.depth + 1} copy={props.copy} defaults={props.defaults} pinned={props.pinned} />}
          </For>
          <Show when={entries().length > shown()}>
            <div class="px-2" style={{ "padding-left": `${24 + props.depth * 16}px` }}>
              <button type="button" class={ui.payloadAction} onClick={() => setShown(shown() + childPage)}>Show {Math.min(childPage, entries().length - shown())} more</button>
              <span class="text-console-subtle"> · {entries().length - shown()} hidden</span>
            </div>
          </Show>
          <div class="text-console-subtle" style={{ "padding-left": `${20 + props.depth * 16}px` }}>{brackets()[1]}</div>
        </div>
      </Show>
    </div>
  );
}

function Scalar(props: { value: JsonValue; copy: Copy }) {
  const [full, setFull] = createSignal(false);
  const long = () => typeof props.value === "string" && props.value.length > stringClamp;
  const tone = () => typeof props.value === "string" ? "text-console-syntax-string" : typeof props.value === "number" ? "text-console-syntax-number" : "text-console-syntax-literal";
  return (
    <>
      <span class={cx(tone(), "whitespace-pre-wrap")}>{full() || !long() ? scalar(props.value) : scalar(props.value, stringClamp)}</span>
      <Show when={long() && !full()}>
        {" "}<button type="button" class={ui.payloadAction} onClick={() => setFull(true)}>Show full string</button>
        <span class="text-console-subtle"> · {formatBytes(byteLength(props.value as string))}</span>
      </Show>
    </>
  );
}

// Plain text output clamped to its first lines, with the rest one click away.
export function TextView(props: { text: string; label: string; class?: string | undefined; inline?: boolean | undefined }) {
  const [full, setFull] = createSignal(false);
  const lines = createMemo(() => props.text.split("\n"));
  const clamped = createMemo(() => {
    const head = lines().slice(0, textLineClamp).join("\n");
    return head.length > stringClamp ? `${head.slice(0, stringClamp)}…` : head;
  });
  const long = () => clamped() !== props.text;
  const lineCount = () => lines().length;
  return (
    <PayloadFrame
      label={props.label}
      class={props.class}
      inline={props.inline}
      meta={`text · ${lineCount()} ${lineCount() === 1 ? "line" : "lines"} · ${formatBytes(byteLength(props.text))}`}
      actions={<CopyAction label="Copy" text={() => props.text} />}
    >
      <div class={cx("whitespace-pre-wrap break-words text-body leading-normal", props.inline ? "py-1 pr-28" : "px-2 py-1.5")}>{full() || !long() ? props.text : clamped()}</div>
      <Show when={long() && !full()}>
        <div class={props.inline ? "pb-1" : "px-2 pb-1.5"}>
          <button type="button" class={ui.payloadAction} onClick={() => setFull(true)}>{lineCount() > textLineClamp ? `Show all ${lineCount()} lines` : "Show all"}</button>
        </div>
      </Show>
    </PayloadFrame>
  );
}
