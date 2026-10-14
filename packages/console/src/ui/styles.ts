export function cx(...classes: Array<string | false | null | undefined>): string {
  return classes.filter(Boolean).join(" ");
}

export type Tone = "info" | "warning" | "success" | "danger" | "neutral";

// Text, background and border of one tone, used together by badges and notices.
export const toneClass: Record<Tone, string> = {
  info: "border-console-info-border bg-console-info-bg text-console-info",
  warning: "border-console-warning-border bg-console-warning-bg text-console-warning",
  success: "border-console-success-border bg-console-success-bg text-console-success",
  danger: "border-console-danger-border bg-console-danger-bg text-console-danger",
  neutral: "border-console-neutral-border bg-console-neutral-bg text-console-neutral",
};

// A button without its size; `buttonBase` is the 28px control size and
// `compactButton` the one 24px size.
const buttonFrame =
  "inline-flex cursor-pointer items-center justify-center gap-1.5 whitespace-nowrap rounded-xs border py-0 font-mono text-chrome font-medium leading-none transition duration-100 disabled:cursor-not-allowed disabled:opacity-45";
const buttonBase = cx(buttonFrame, "h-7 min-h-7 px-2.5");
const secondaryTone =
  "border-console-border gradient-button text-console-text hover:border-console-border-strong hover:gradient-button-hover hover:text-console-text";

const closeButtonBase =
  "grid shrink-0 cursor-pointer place-items-center rounded-xs border border-transparent bg-transparent p-0 text-console-muted transition hover:border-console-border hover:bg-console-surface hover:text-console-text disabled:cursor-not-allowed disabled:opacity-45";

// The header of a container: 32px like a table's column header row. The
// base sets no colour; add the default or danger variant.
const containerHeaderBase = "flex h-8 min-h-8 shrink-0 items-center justify-between gap-2 border-b px-3";

// Border, focus ring and invalid state shared by every form control; each control
// adds its own size and font.
const controlFrame =
  "rounded-xs border border-console-border text-console-text outline-none transition hover:border-console-border-strong focus:border-console-accent focus:shadow-console-control-focus aria-[invalid=true]:border-console-danger-outline aria-[invalid=true]:shadow-console-control-invalid";

export const ui = {
  // Type and labels
  microLabel: "font-mono text-micro font-medium uppercase tracking-[0.06em] text-console-subtle",
  h1: "m-0 text-[1.32rem] font-medium leading-[1.12] text-console-text",
  h2: "m-0 text-[0.9rem] font-medium leading-tight text-console-text",
  muted: "text-body text-console-muted",
  monoLink: "cursor-pointer font-mono text-chrome text-console-accent hover:text-console-accent-hover",

  // Spacing rules: buttons in a group sit 8px apart, page sections stack at 24px,
  // blocks inside a container, dialog or list row sit 12px apart.
  buttonGroup: "flex flex-wrap items-center gap-2",
  sectionStack: "flex flex-col gap-6",
  blockStack: "flex flex-col gap-3",

  // Containers, in the page (Panel, DataTable, List, KeyValueList, DangerZone) and
  // floating (Popover, Dialog, AuthWindow): one border, no shadow, one header.
  container: "border border-console-border-strong bg-console-surface",
  containerHeader: cx(containerHeaderBase, "border-console-border bg-console-bg-panel"),
  containerHeaderDanger: cx(containerHeaderBase, "border-console-danger-border bg-console-danger-bg"),
  containerTitle: "m-0 min-w-0 truncate text-body font-medium leading-snug",
  containerCount: "ml-2 font-mono text-chrome font-normal text-console-subtle",
  containerActions: "flex shrink-0 items-center gap-2",
  // The 12px gap is the only spacing between the body's blocks: direct children
  // drop their vertical margins (QueryState's refetch notice keeps its margin in
  // page flow). A table or list (`data-list-frame`) that is a direct child loses
  // its own frame: it runs to the container's edges, with a divider above and
  // below only where other blocks sit. Nested deeper, it keeps its frame.
  containerBody:
    "flex flex-col gap-3 p-3 [&>*]:my-0 [&>[data-list-frame]]:-mx-3 [&>[data-list-frame]]:border-x-0 [&>[data-list-frame]]:border-console-border [&>[data-list-frame]:first-child]:-mt-3 [&>[data-list-frame]:first-child]:border-t-0 [&>[data-list-frame]:last-child]:-mb-3 [&>[data-list-frame]:last-child]:border-b-0",

  // Navigation
  settingsNavLink:
    "relative flex h-7.5 cursor-pointer items-center rounded-xs px-2.25 font-mono text-chrome font-medium text-console-muted transition-colors",
  settingsNavLinkHover: "hover:bg-console-surface-muted hover:text-console-text",
  settingsNavLinkActive:
    "bg-console-accent-soft text-console-text before:absolute before:-left-2.5 before:top-1/2 before:h-4 before:w-px before:-translate-y-1/2 before:bg-console-accent max-[960px]:before:hidden",

  // Phone menu drawer: 44px targets. The base sets no colour; add the idle or
  // active variant.
  drawerItem:
    "flex h-11 min-h-11 w-full cursor-pointer items-center gap-2 rounded-xs border-0 px-2.5 text-left font-mono text-control font-medium transition-colors disabled:cursor-not-allowed disabled:opacity-45",
  drawerItemIdle: "bg-transparent text-console-muted hover:bg-console-bg-panel hover:text-console-text",
  drawerItemActive: "bg-console-accent-soft text-console-text shadow-[inset_2px_0_0_var(--console-accent)]",

  // Environment rows in the Settings Environment rail
  scopeItem:
    "grid min-h-6 w-full cursor-pointer grid-cols-[12px_minmax(0,1fr)_auto] items-center gap-2 rounded-xs border border-transparent bg-transparent px-2 py-0.5 text-left font-mono text-badge font-medium text-console-text transition-colors hover:border-console-border hover:bg-console-bg-panel",
  scopeItemSelected:
    "border-console-accent bg-console-accent-soft text-console-text hover:border-console-accent hover:bg-console-accent-soft",
  // A full-width text action inside a popover (profile menu).
  scopeAction:
    "flex h-7 w-full cursor-pointer items-center justify-start gap-2 rounded-xs border-0 bg-transparent px-2 font-mono text-badge font-medium text-console-muted transition hover:bg-console-surface hover:text-console-text disabled:cursor-not-allowed disabled:opacity-45",

  // Buttons
  button: cx(
    buttonBase,
    "border-console-accent bg-console-accent text-white hover:border-console-accent-hover hover:bg-console-accent-hover active:translate-y-px",
  ),
  secondaryButton: cx(buttonBase, secondaryTone),
  // The one 24px button: container header actions, table and list footers,
  // notice actions, a code block's Copy and a queue row's action.
  compactButton: cx(buttonFrame, "h-6 min-h-6 px-2", secondaryTone),
  dangerOutlineButton: cx(
    buttonBase,
    "border-console-danger-outline bg-console-danger-bg text-console-danger hover:border-console-danger-solid hover:bg-console-danger-solid hover:text-white",
  ),
  // A square 28px icon button for bar and row chrome (settings, row menus).
  iconButton:
    "inline-grid size-7 shrink-0 cursor-pointer place-items-center rounded-xs border border-transparent bg-transparent p-0 leading-none text-console-muted transition duration-100 hover:border-console-border-strong hover:bg-console-bg-panel hover:text-console-text disabled:cursor-not-allowed disabled:opacity-45 data-[open=true]:border-console-border-strong data-[open=true]:bg-console-bg-panel data-[open=true]:text-console-text",
  // The close control: 24px in a container header (dialogs), 20px in a
  // dismissible notice. The base sets no size.
  closeButton: cx(closeButtonBase, "size-6"),
  noticeCloseButton: cx(closeButtonBase, "size-5"),
  closeIcon:
    "relative block size-2.5 before:absolute before:left-1/2 before:top-0 before:h-full before:w-px before:-translate-x-1/2 before:rotate-45 before:bg-current before:content-[''] after:absolute after:left-1/2 after:top-0 after:h-full after:w-px after:-translate-x-1/2 after:-rotate-45 after:bg-current after:content-['']",

  // Popovers and menus: the container border, no shadow.
  popover: "fixed z-50 flex flex-col overflow-hidden border border-console-border-strong bg-console-surface",
  // The base sets no colour: two Tailwind colour utilities on one element resolve
  // by stylesheet order, so only the tone variant may set `text-*`.
  actionMenuItemBase:
    "flex min-h-7 w-full cursor-pointer items-center justify-start whitespace-nowrap rounded-xs border border-transparent bg-transparent px-2.5 py-1 text-left font-mono text-chrome font-medium leading-snug transition-colors focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-45",
  actionMenuItemDefault:
    "text-console-text hover:border-console-border hover:bg-console-bg-panel hover:text-console-text focus-visible:border-console-border focus-visible:bg-console-bg-panel focus-visible:text-console-text",
  actionMenuItemDanger:
    "text-console-danger-text hover:border-console-danger-outline hover:bg-console-danger-bg hover:text-console-danger-text focus-visible:border-console-danger-outline focus-visible:bg-console-danger-bg focus-visible:text-console-danger-text",
  actionMenuSeparator: "my-1 border-t border-console-border",
  selectTrigger: cx(
    "flex h-7 min-h-7 min-w-full cursor-pointer items-center justify-between gap-2 bg-console-surface-raised px-2.5 py-0 text-left text-control leading-none hover:bg-console-surface focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50 data-[open=true]:border-console-border-strong",
    controlFrame,
  ),
  selectOption:
    "grid min-h-6 w-full cursor-pointer grid-cols-[16px_minmax(0,1fr)_auto] items-center gap-2 rounded-xs border border-transparent bg-transparent px-2 py-0.5 text-left text-control text-console-text transition-colors hover:border-console-border hover:bg-console-bg-panel focus-visible:outline-none focus-visible:border-console-border focus-visible:bg-console-bg-panel",
  selectedOption:
    "border-console-accent bg-console-accent-soft text-console-text hover:border-console-accent hover:bg-console-accent-soft",

  // Form controls
  input: cx("h-7 min-h-7 w-full bg-console-surface px-2.5 py-0 text-control leading-none placeholder:text-console-faint", controlFrame),
  monoInput: cx("h-7 min-h-7 w-full bg-console-surface px-2.5 py-0 font-mono text-chrome leading-none placeholder:text-console-faint", controlFrame),
  textarea: cx("min-h-17 w-full resize-y bg-console-surface px-2.5 py-2 text-control leading-snug placeholder:text-console-faint", controlFrame),
  fieldLabel: "text-chrome font-medium text-console-text",
  fieldHint: "text-chrome font-normal leading-snug text-console-muted",
  fieldSet: "mb-3 mt-1 border-0 p-0",
  fieldLegend: "mb-2 p-0 text-control font-medium text-console-text",
  segment:
    "h-6.5 cursor-pointer whitespace-nowrap border-0 border-l border-console-border-strong gradient-button px-2.5 font-mono text-chrome font-medium text-console-text first:border-l-0 hover:gradient-button-hover disabled:cursor-not-allowed disabled:opacity-45 aria-pressed:gradient-pressed aria-pressed:shadow-console-pressed",
  choiceCard:
    "grid cursor-pointer grid-cols-[15px_1fr] gap-2 border border-console-border bg-console-bg-panel px-2.5 py-2 transition hover:border-console-border-strong hover:bg-console-surface has-checked:border-console-accent has-checked:bg-console-accent-soft [&_input]:mt-px [&_input]:size-[15px] [&_input]:accent-console-accent [&_span]:block [&_strong]:block [&_strong]:text-control [&_strong]:font-medium [&_strong]:text-console-text [&_span_span]:mt-0.5 [&_span_span]:text-chrome [&_span_span]:font-normal [&_span_span]:leading-snug [&_span_span]:text-console-muted",

  // Page structure
  page: "mx-auto w-full max-w-310 px-5 pb-12 pt-10",
  pageHeader: "mb-4 flex items-start justify-between gap-4 max-sm:flex-col max-sm:items-stretch",
  pageTitle: "flex items-center gap-2.5",
  pageSubtitle: "mt-1.5 max-w-180 text-body leading-normal text-console-muted",
  toolbar: "mb-3 flex flex-wrap items-center justify-between gap-2",
  toolbarSide: "flex flex-wrap items-center gap-2",
  modalActions: "mt-3 flex flex-wrap justify-end gap-2",
  modalIntro: "mb-3 text-body leading-normal text-console-muted",

  // Tables
  tableWrap:
    "overflow-x-auto [scrollbar-color:var(--console-scrollbar)_var(--console-surface)] [scrollbar-width:thin] [&::-webkit-scrollbar]:h-2 [&::-webkit-scrollbar-track]:bg-console-surface [&::-webkit-scrollbar-thumb]:rounded-full [&::-webkit-scrollbar-thumb]:border-2 [&::-webkit-scrollbar-thumb]:border-solid [&::-webkit-scrollbar-thumb]:border-console-surface [&::-webkit-scrollbar-thumb]:bg-slate-300 [&::-webkit-scrollbar-thumb:hover]:bg-slate-400 [&_table]:w-full [&_table]:border-separate [&_table]:border-spacing-0 [&_thead_th]:sticky [&_thead_th]:top-0 [&_thead_th]:z-10 [&_thead_th]:h-8 [&_thead_th]:border-b [&_thead_th]:border-console-border [&_thead_th]:bg-console-bg-panel [&_thead_th]:px-3 [&_thead_th]:py-0 [&_thead_th]:text-left [&_thead_th]:font-mono [&_thead_th]:text-micro [&_thead_th]:font-medium [&_thead_th]:uppercase [&_thead_th]:tracking-[0.06em] [&_thead_th]:text-console-subtle [&_tbody_tr]:h-9 [&_tbody_tr]:transition-colors [&_tbody_tr:hover]:bg-console-bg-panel [&_tbody_tr:last-child_td]:border-b-0 [&_tbody_td]:h-9 [&_tbody_td]:whitespace-nowrap [&_tbody_td]:border-b [&_tbody_td]:border-console-border-soft [&_tbody_td]:px-3 [&_tbody_td]:py-0 [&_tbody_td]:align-middle [&_tbody_td]:text-body [&_tbody_td]:text-console-text [&_tbody_td_code]:font-mono [&_tbody_td_code]:text-chrome [&_tbody_td_code]:text-console-muted",
  tableFooter:
    "flex h-9 items-center gap-3 border-t border-console-border bg-console-bg-panel px-3 font-mono text-chrome text-console-muted",
  actionsCell: "w-px whitespace-nowrap",
  emptyCell: "text-console-faint",

  // Feedback
  error: "mt-3 text-body font-medium text-console-danger",
  emptyState:
    "m-0 flex flex-col items-center gap-1.5 border border-dashed border-console-border bg-console-bg-panel px-5 py-7 text-center text-body text-console-muted",
  notice: "flex items-center gap-2.5 border px-3 py-2 text-body leading-normal",
  noticeBar: "flex items-center gap-3 border-b px-5 py-1.5 text-body leading-normal max-[860px]:px-3",

  // Code and values
  codeBlock:
    "m-0 max-h-130 overflow-auto whitespace-pre-wrap break-words border border-console-border bg-console-bg-panel px-4 py-3 font-mono text-control leading-normal text-console-text",

  // Auth windows
  authTitle: "mb-2 text-[1.2rem] font-medium leading-tight text-console-text",
  authCopy: "mb-3.5 text-body leading-normal text-console-muted",
  authActions: "mt-3 flex flex-col gap-1.5",
  authDivider:
    "my-3.5 flex items-center gap-3 font-mono text-micro font-medium uppercase tracking-[0.06em] text-console-subtle [&>span:first-child]:h-px [&>span:first-child]:flex-1 [&>span:first-child]:bg-console-border [&>span:last-child]:h-px [&>span:last-child]:flex-1 [&>span:last-child]:bg-console-border",
  authCode:
    "my-3.5 block w-full border border-console-border-strong bg-console-bg-panel p-3 text-center font-mono text-[1.25rem] font-medium tracking-[0.16em] text-console-accent",
};

export function actionMenuItemClass(tone: "default" | "danger" | undefined): string {
  return cx(ui.actionMenuItemBase, tone === "danger" ? ui.actionMenuItemDanger : ui.actionMenuItemDefault);
}
