export function cx(...classes: Array<string | false | null | undefined>): string {
  return classes.filter(Boolean).join(" ");
}

export const ui = {
  brandMark:
    "grid size-4 shrink-0 place-items-center rounded-xs border border-console-border-strong bg-transparent text-console-text",
  tabLink:
    "relative flex h-6.5 cursor-pointer items-center rounded-xs border border-transparent px-2.5 font-mono text-[11.5px] font-medium text-console-muted transition duration-100",
  tabLinkHover:
    "hover:border-console-border-strong hover:bg-console-surface-muted hover:text-console-text",
  tabLinkActive:
    "border-transparent bg-transparent text-console-text after:absolute after:bottom-0.75 after:left-2.5 after:right-2.5 after:h-px after:bg-console-accent",
  settingsNavLink:
    "relative flex h-7.5 cursor-pointer items-center rounded-xs px-2.25 font-mono text-[11.5px] font-medium text-console-muted transition-colors",
  settingsNavLinkHover:
    "hover:bg-console-surface-muted hover:text-console-text",
  settingsNavLinkActive:
    "bg-console-accent-soft text-console-text before:absolute before:-left-2.5 before:top-1/2 before:h-4 before:w-px before:-translate-y-1/2 before:bg-console-accent max-[960px]:before:hidden",
  selectTrigger:
    "flex h-7 min-w-full cursor-pointer items-center justify-between gap-2 rounded-xs border border-console-border bg-console-surface-raised px-2.5 py-0 text-left font-mono text-[11.5px] font-medium text-console-text transition hover:border-console-border-strong hover:bg-white focus-visible:outline-2 focus-visible:outline-console-accent-soft disabled:cursor-not-allowed disabled:opacity-50",
  selectMenu:
    "fixed z-50 max-h-[min(260px,calc(100vh-16px))] max-w-[calc(100vw-16px)] overflow-y-auto rounded-[3px] border border-console-border-strong bg-console-surface p-0.75 shadow-[0_16px_36px_rgb(15_23_42/0.12)]",
  selectOption:
    "grid min-h-6 w-full cursor-pointer grid-cols-[16px_minmax(0,1fr)_auto] items-center gap-2 rounded-xs border border-transparent bg-transparent px-2 py-0.5 text-left font-mono text-[11px] font-medium text-console-text transition-colors hover:border-console-border hover:bg-console-bg-panel",
  selectedOption:
    "border-console-accent bg-console-accent-soft text-console-text hover:border-console-accent hover:bg-console-accent-soft",
  scopeTrigger:
    "flex h-6.5 cursor-pointer max-w-107.5 items-center gap-1.5 rounded-xs border border-transparent bg-transparent px-2 py-0 font-mono text-[11px] text-console-text transition hover:border-console-border-strong hover:bg-console-bg-panel hover:text-console-text disabled:cursor-not-allowed disabled:opacity-50 data-[open=true]:border-console-border-strong data-[open=true]:bg-console-bg-panel",
  scopeItem:
    "grid min-h-6 w-full cursor-pointer grid-cols-[12px_minmax(0,1fr)_auto] items-center gap-2 rounded-xs border border-transparent bg-transparent px-2 py-0.5 text-left font-mono text-[11px] font-medium text-console-text transition-colors hover:border-console-border hover:bg-console-bg-panel",
  scopeItemPreviewed: "border-console-border bg-console-bg-panel text-console-text",
  scopeItemSelected:
    "border-console-accent bg-console-accent-soft text-console-text hover:border-console-accent hover:bg-console-accent-soft",
  scopeAction:
    "flex h-7 w-full cursor-pointer items-center justify-start gap-2 rounded-xs border-0 bg-transparent px-2 font-mono text-[11px] font-medium text-console-muted transition hover:bg-white hover:text-console-text disabled:cursor-not-allowed disabled:opacity-45",
  logTab:
    "relative inline-flex h-9 cursor-pointer items-center gap-1.5 border-0 bg-transparent px-3 py-0 font-mono text-[11.5px] font-medium text-console-muted transition-colors hover:text-console-text",
  logTabActive:
    "text-console-text after:absolute after:-bottom-px after:left-3 after:right-3 after:h-px after:bg-console-accent",
  button:
    "inline-flex h-7 min-h-7 cursor-pointer items-center justify-center gap-1.5 whitespace-nowrap rounded-xs border border-console-accent bg-console-accent px-2.5 py-0 font-mono text-[11.5px] font-medium leading-none text-white transition duration-100 hover:border-console-accent-hover hover:bg-console-accent-hover active:translate-y-px disabled:cursor-not-allowed disabled:opacity-45",
  secondaryButton:
    "inline-flex h-7 min-h-7 cursor-pointer items-center justify-center gap-1.5 whitespace-nowrap rounded-xs border border-console-border bg-[linear-gradient(to_bottom,#fbfcfd_0%,#eef1f3_100%)] px-2.5 py-0 font-mono text-[11.5px] font-medium leading-none text-console-text transition duration-100 hover:border-console-border-strong hover:bg-[linear-gradient(to_bottom,#f3f5f7_0%,#dfe5ea_100%)] hover:text-console-text disabled:cursor-not-allowed disabled:opacity-45",
  ghostButton:
    "inline-flex h-7 min-h-7 cursor-pointer items-center justify-center gap-1.5 whitespace-nowrap rounded-xs border border-transparent bg-transparent px-2 py-0 font-mono text-[11.5px] font-medium leading-none text-console-muted transition duration-100 hover:border-console-border-strong hover:bg-console-bg-panel hover:text-console-text disabled:cursor-not-allowed disabled:opacity-45",
  dangerButton:
    "inline-flex h-7 min-h-7 cursor-pointer items-center justify-center gap-1.5 whitespace-nowrap rounded-xs border border-console-danger bg-console-danger px-2.5 py-0 font-mono text-[11.5px] font-medium leading-none text-white transition duration-100 hover:border-[#9f312b] hover:bg-[#9f312b] disabled:cursor-not-allowed disabled:opacity-45",
  iconButton:
    "inline-flex size-5 cursor-pointer items-center justify-center rounded-xs border-0 bg-transparent p-0 text-base leading-none text-console-muted transition duration-100 hover:bg-console-bg-panel hover:text-console-text",
  actionMenuTrigger:
    "inline-flex h-7 min-h-7 cursor-pointer items-center justify-center gap-1 rounded-xs border border-console-border bg-[linear-gradient(to_bottom,#fbfcfd_0%,#eef1f3_100%)] px-2 py-0 font-mono text-[11px] font-medium leading-none text-console-muted transition duration-100 hover:border-console-border-strong hover:bg-[linear-gradient(to_bottom,#f3f5f7_0%,#dfe5ea_100%)] hover:text-console-text focus-visible:outline-2 focus-visible:outline-console-accent-soft disabled:cursor-not-allowed disabled:opacity-45 data-[open=true]:border-console-border-strong data-[open=true]:bg-console-bg-panel data-[open=true]:text-console-text",
  actionMenuCaret:
    "mt-px size-0 border-x-[3px] border-t-[4px] border-x-transparent border-t-current",
  actionMenu:
    "fixed z-50 max-h-[min(260px,calc(100vh-16px))] overflow-y-auto rounded-xs border border-console-border-strong bg-console-surface p-0.75 shadow-[0_16px_36px_rgb(15_23_42/0.14)]",
  actionMenuItem:
    "flex min-h-7 w-full cursor-pointer items-center justify-start rounded-xs border border-transparent bg-transparent px-2.5 py-1 text-left font-mono text-[11.5px] font-medium leading-snug text-console-text transition-colors hover:border-console-border hover:bg-console-bg-panel disabled:cursor-not-allowed disabled:opacity-45",
  actionMenuItemDanger: "text-console-danger hover:text-console-danger",
  input:
    "h-7 min-h-7 w-full rounded-xs border border-console-border bg-white px-2.5 py-0 text-[12px] leading-none text-console-text outline-none transition placeholder:text-console-faint hover:border-console-border-strong focus:border-console-accent focus:shadow-[0_0_0_2px_rgb(49_95_206/0.12)]",
  textarea:
    "min-h-17 w-full resize-y rounded-xs border border-console-border bg-white px-2.5 py-2 text-[12px] leading-snug text-console-text outline-none transition placeholder:text-console-faint hover:border-console-border-strong focus:border-console-accent focus:shadow-[0_0_0_2px_rgb(49_95_206/0.12)]",
  field:
    "mb-3 grid gap-1 text-[11.5px] font-medium text-console-muted [&>span]:font-medium [&>span]:text-console-text",
  fieldError: "-mt-1 text-xs font-medium text-console-danger",
  fieldSet: "mb-3 mt-1 border-0 p-0",
  fieldLegend: "mb-2 p-0 text-[12px] font-medium text-console-text",
  h1: "m-0 text-[1.32rem] font-medium leading-[1.12] text-console-text",
  h2: "m-0 text-[0.9rem] font-medium leading-tight text-console-text",
  h3: "m-0 font-mono text-[0.68rem] font-medium uppercase tracking-[0.06em] text-console-subtle",
  page: "mx-auto w-full max-w-310 px-5 pb-12 pt-10",
  pageHeader: "mb-4 flex items-start justify-between gap-4 max-sm:flex-col max-sm:items-stretch",
  pageTitle: "flex items-center gap-2.5",
  pageSubtitle: "mt-1.5 max-w-180 text-[12.5px] leading-normal text-console-muted",
  backLink:
    "mb-2 inline-flex cursor-pointer items-center gap-1 font-mono text-xs font-medium text-console-muted before:text-console-subtle before:content-['←'] hover:text-console-text",
  toolbar: "mb-3 flex flex-wrap items-center justify-between gap-2.5",
  toolbarSide: "flex flex-wrap items-center gap-2",
  filterField: "inline-flex items-center gap-2 font-mono text-[11.5px] font-medium text-console-muted",
  metricStrip: "mb-4 grid grid-cols-4 gap-2 max-[960px]:grid-cols-2 max-sm:grid-cols-2",
  metricCard:
    "border border-console-border bg-console-surface-raised px-3 py-2.5 transition-colors hover:border-console-border-strong [&>span]:block [&>span]:font-mono [&>span]:text-[10.5px] [&>span]:font-medium [&>span]:text-console-subtle [&>strong]:mt-1 [&>strong]:block [&>strong]:text-[1.25rem] [&>strong]:font-medium [&>strong]:leading-none",
  tableWrap:
    "overflow-x-auto border border-console-border-strong bg-console-surface [scrollbar-color:rgba(15,23,42,0.28)_#ffffff] [scrollbar-width:thin] [&::-webkit-scrollbar]:h-2 [&::-webkit-scrollbar-track]:bg-white [&::-webkit-scrollbar-thumb]:rounded-full [&::-webkit-scrollbar-thumb]:border-2 [&::-webkit-scrollbar-thumb]:border-solid [&::-webkit-scrollbar-thumb]:border-white [&::-webkit-scrollbar-thumb]:bg-slate-300 [&::-webkit-scrollbar-thumb:hover]:bg-slate-400 [&_table]:w-full [&_table]:border-separate [&_table]:border-spacing-0 [&_thead_th]:sticky [&_thead_th]:top-0 [&_thead_th]:z-10 [&_thead_th]:h-8 [&_thead_th]:border-b [&_thead_th]:border-console-border [&_thead_th]:bg-console-bg-panel [&_thead_th]:px-3 [&_thead_th]:py-0 [&_thead_th]:text-left [&_thead_th]:font-mono [&_thead_th]:text-[10px] [&_thead_th]:font-medium [&_thead_th]:uppercase [&_thead_th]:tracking-[0.06em] [&_thead_th]:text-console-subtle [&_tbody_tr]:h-9 [&_tbody_tr]:transition-colors [&_tbody_tr:hover]:bg-console-bg-panel [&_tbody_tr:last-child_td]:border-b-0 [&_tbody_td]:h-9 [&_tbody_td]:whitespace-nowrap [&_tbody_td]:border-b [&_tbody_td]:border-console-border-soft [&_tbody_td]:px-3 [&_tbody_td]:py-0 [&_tbody_td]:align-middle [&_tbody_td]:text-[12.5px] [&_tbody_td]:text-console-text [&_tbody_td_code]:font-mono [&_tbody_td_code]:text-[11.5px] [&_tbody_td_code]:text-console-muted",
  clickableTableRow:
    "cursor-pointer focus-visible:bg-console-bg-panel focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-console-accent hover:bg-console-bg-panel",
  detailTableRow:
    "!h-12 [&>td]:!h-12 [&>td]:!py-1.5",
  tableCellStack:
    "grid gap-0.5 [&>strong]:block [&>strong]:font-medium [&>strong]:leading-tight [&>div]:leading-tight",
  dataTable: "min-w-225",
  apiKeyTable: "min-w-270",
  actionsCell: "w-px whitespace-nowrap",
  error: "mt-3 text-[12.5px] font-medium text-console-danger",
  rowError: "mt-1.5 text-xs font-medium leading-snug text-console-danger",
  muted: "text-[12.5px] text-console-muted",
  emptyState:
    "m-0 flex flex-col items-center gap-1.5 border border-dashed border-console-border bg-console-bg-panel px-5 py-7 text-center text-[12.5px] text-console-muted",
  hasMoreBanner:
    "mb-3 border border-[#d6a33f]/35 bg-[#fff7df] px-3 py-2 text-[12.5px] text-[#7b5a12]",
  inlineState: "mt-2.5 text-[12.5px] font-medium text-console-accent",
  modalBackdrop: "fixed inset-0 z-50 grid place-items-center bg-slate-950/30 p-3 backdrop-blur-[2px]",
  modal:
    "flex max-h-[min(82vh,620px)] w-full max-w-115 flex-col overflow-hidden border border-console-border-strong bg-console-surface shadow-[2px_2px_0_rgb(15_23_42/0.12)] outline-none",
  modalHeader:
    "flex min-h-9 shrink-0 items-center justify-between gap-3 border-b border-console-border bg-console-bg-panel px-3.5 py-2.5",
  modalTitle: "m-0 min-w-0 flex-1 truncate font-mono text-[11.5px] font-medium text-console-text",
  modalCloseButton:
    "grid size-5 shrink-0 cursor-pointer place-items-center rounded-xs border border-transparent bg-transparent p-0 text-console-muted transition hover:border-console-border hover:bg-white hover:text-console-text disabled:cursor-not-allowed disabled:opacity-45",
  modalCloseIcon:
    "relative block size-2.5 before:absolute before:left-1/2 before:top-0 before:h-full before:w-px before:-translate-x-1/2 before:rotate-45 before:bg-current before:content-[''] after:absolute after:left-1/2 after:top-0 after:h-full after:w-px after:-translate-x-1/2 after:-rotate-45 after:bg-current after:content-['']",
  modalBody: "min-h-0 overflow-y-auto px-3.5 py-3.5",
  modalIntro: "mb-3 text-[12.5px] leading-normal text-console-muted",
  actionRow: "mt-3 flex flex-wrap justify-end gap-1.5",
  modalActions: "mt-3 flex flex-wrap justify-end gap-1.5",
  warning:
    "mb-3 border border-[#d6a33f]/35 bg-[#fff7df] px-3 py-2 text-[12.5px] leading-normal text-[#7b5a12]",
  rawKey:
    "block select-all overflow-x-auto whitespace-nowrap border border-console-border bg-console-bg-panel p-3 font-mono text-[12px] leading-normal text-console-text",
  scopeSummary:
    "mb-3 grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 border border-console-border bg-console-bg-panel px-3 py-2.5 text-[12px] [&_span]:font-mono [&_span]:text-[10.5px] [&_span]:font-medium [&_span]:uppercase [&_span]:tracking-[0.04em] [&_span]:text-console-subtle [&_strong]:font-medium [&_strong]:text-console-text",
  permissionOption:
    "grid cursor-pointer grid-cols-[15px_1fr] gap-2 border border-console-border bg-console-bg-panel px-2.5 py-2 transition hover:border-console-border-strong hover:bg-white [&_input]:mt-px [&_input]:size-[15px] [&_input]:accent-console-accent [&_span]:block [&_strong]:block [&_strong]:text-[12px] [&_strong]:font-medium [&_strong]:text-console-text [&_span_span]:mt-0.5 [&_span_span]:text-[11.5px] [&_span_span]:font-normal [&_span_span]:leading-snug [&_span_span]:text-console-muted",
  panel:
    "w-full max-w-98 border border-console-border-strong bg-console-surface px-4 pb-4 pt-4 text-console-text shadow-[2px_2px_0_rgb(15_23_42/0.12)] [&_a]:mt-3 [&_a]:inline-block [&_a]:cursor-pointer [&_a]:text-[12.5px] [&_a]:text-console-accent [&_a:hover]:text-console-accent-hover [&_button]:w-full [&_p]:text-[12.5px] [&_p]:leading-normal [&_p]:text-console-muted",
  authTitle: "mb-2 text-[1.2rem] font-medium leading-tight text-console-text",
  authCopy: "mb-3.5 text-[12.5px] leading-normal text-console-muted",
  authActions: "mt-3 flex flex-col gap-1.5",
  authDivider:
    "my-3.5 flex items-center gap-3 font-mono text-[10.5px] font-medium uppercase tracking-[0.06em] text-console-subtle [&>span:first-child]:h-px [&>span:first-child]:flex-1 [&>span:first-child]:bg-console-border [&>span:last-child]:h-px [&>span:last-child]:flex-1 [&>span:last-child]:bg-console-border",
  authCode:
    "my-3.5 block w-full border border-console-border-strong bg-console-bg-panel p-3 text-center font-mono text-[1.25rem] font-medium tracking-[0.16em] text-console-accent",
  authStatus: "mb-2 text-[12.5px] font-medium text-console-text",
};

export function statusBadgeClass(tone: "active" | "waiting" | "succeeded" | "revoked" | "expired"): string {
  const tones = {
    active: "border-[#9bb9e8] bg-[#eef4ff] text-console-info",
    waiting: "border-[#e5c26e] bg-[#fff7df] text-console-warning before:animate-pulse",
    succeeded: "border-[#a8c3ad] bg-[#eef7f0] text-console-success",
    revoked: "border-[#e6aaa4] bg-[#fff1ef] text-console-danger",
    expired: "border-console-border bg-console-bg-panel text-console-muted",
  };
  return cx(
    "inline-flex items-center gap-1.5 whitespace-nowrap rounded-xs border px-2 py-0.5 font-mono text-[11px] font-medium leading-normal before:size-1.5 before:bg-current before:content-['']",
    tones[tone],
  );
}

export function envDotClass(tone: "danger" | "warning" | "info" | "success" | "neutral"): string {
  const tones = {
    danger: "bg-console-danger",
    warning: "bg-console-warning",
    info: "bg-console-info",
    success: "bg-console-success",
    neutral: "bg-console-faint",
  };
  return cx("inline-block size-1.5 shrink-0 rounded-full", tones[tone]);
}
