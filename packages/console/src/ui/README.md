# Console UI conventions

The console UI is intentionally compact, square, and work-focused.

- Prefer existing primitives in `src/ui` before adding page-local class strings.
- Keep form controls aligned at `28px` high: `ui.button`, `ui.secondaryButton`, `ui.input`, and `ui.selectTrigger`.
- Use `AuthScreen`, `AuthTitle`, `AuthCopy`, `AuthDivider`, and `AuthActions` for auth/device/callback screens.
- Use `Modal` for dialogs so outside click, Escape, close button, sizing, and header treatment stay consistent.
- Use `ConfirmModal` for cancel, promote, and other confirmations so the button order, busy state, and error placement stay the same.
- Use `ui.actionRow` for ordinary action rows and `ui.modalActions` only inside modals.
- Buttons: `ui.button` for the primary action, `ui.secondaryButton` for the rest, `ui.dangerOutlineButton` for destructive header and modal actions, `ui.ghostButton` for the quiet "view all" links in section headers and modal footers. Table rows never carry buttons.
- Use `ui.field`, `ui.fieldError`, `ui.fieldSet`, and `ui.fieldLegend` for forms instead of page-local label and legend classes.
- Use `ui.codeBlock` for a `<pre>` of JSON, stdout, or stderr on detail pages.
- Use semantic Tailwind tokens from `styles.css` for new one-off layouts, for example `bg-console-surface`, `border-console-border`, `text-console-muted`, `font-console-mono`.
- Keep arbitrary values for exact UI details that are part of the console visual language, such as `rounded-xs` or fixed table heights.

When a repeated style includes structure or behavior, make a Solid component. When it is a single repeated element style, add a `ui` token. Avoid broad descendant overrides that silently change nested controls.

## IDs and actions

The Founder decided where IDs and actions live (2026-09-12). Apply the table to every page instead of choosing per page.

| Place | IDs | Actions |
| --- | --- | --- |
| List rows | Prefer a human-readable identifier (entrypoint, key, version, actor id). Keep an ID column only where nothing better exists (Runs, Tokens, Workspaces), rendered with `IDText` short form: last hyphen segment of the UUID, mono muted, full ID in `title`. No copy control. | None. Rows navigate to the detail page: the primary cell is the link (`IDText mode="link"` or an `<A>` on the readable identifier). Rarely used actions go into `ActionMenu` in a trailing `{ label: "Actions", srOnly: true }` column. Never render a menu whose only items duplicate the primary cell's navigation. |
| Detail pages | The full ID once, under the title, as `PageHeader` `subtitle` with `IDText mode="full"` (click-to-copy). Rail entries for related resources use `IDText mode="link"`, so long UUIDs never wrap. | Every action in the `PageHeader` `actions` slot, confirmed with `ConfirmModal` or an input `Modal`. |
| Overview sections | Same as list rows: short form (`IDText mode="link"` to the detail page) or the readable identifier as the link. | Same as list rows: no inline buttons. "Needs you" Token rows offer "Complete" and "Cancel", and "In progress" rows "Cancel run", through the row `ActionMenu` (the Actor row has no menu because its item cell already opens the Session), with the same gating and modals as the detail pages; the confirm or complete modal shows what is being acted on (token id, tags, metadata when available). |

- `ActionMenu` — the row menu: an icon-only 28px kebab trigger whose `aria-label` is `Actions for <target>`. Item labels are sentence-case verbs with no trailing ellipsis ("Complete", "Cancel run", "Promote", "Delete"). Pass `href` for navigation and `tone: "danger"` for destructive items (rendered in `text-console-danger-text`, the red for danger text on an untinted surface); the menu orders navigation, then ordinary actions, then the danger group after a separator, and supports ArrowUp/ArrowDown/Home/End, Enter/Space and Escape.

## Page and section structure

- `PageHeader` — title, optional badge, subtitle, back link, and right-aligned actions. Every route page starts with it.
- `SectionHeader` — an `h2` with optional count, subtitle, and "view all" style actions for sections inside a page.
- `Panel`, `DetailList`, `DetailItem` — detail pages: a bordered panel with a heading, and the right-hand rail of label/value pairs.
- `StatePanel` — the only loading, empty, and error rendering: `<StatePanel loading="…" />`, `<StatePanel error="…" />`, `<StatePanel empty="…" hint="…">optional action</StatePanel>`.

## Tables

- `DataTable` — the only table wrapper. Pass `columns` (strings, or `{ label, srOnly }` for action columns) and render `<tr>` rows as children. Header, cell, and row hover styling live in `ui.tableWrap`; pass `minWidth` (a Tailwind `min-w-*` class) when the table needs horizontal scroll on narrow screens.
- Rows are single-line. Secondary information (kind, tags, IDs, failure messages) gets its own column instead of a second line inside a cell. There is no two-line cell primitive; if a cell would wrap, add a column.
- `StatusBadge` — `<StatusBadge resource="run" status={run.status} />`. One tone map per resource lives in `StatusBadge.tsx`; add a resource there instead of styling a badge locally. Labels are derived from the status value.
- `IDText` — every ID and digest: monospace, muted, full value in `title`. Three modes, and the copy affordance exists only in `full`:
  - `short` (default): the short form as plain text. `formatID` in `ui/id.ts` is the shared rule: the last hyphen segment of a UUID (12 hex characters), `algo:` plus the last 12 characters of a digest, no leading ellipsis; any other value is shown unchanged.
  - `link` with `href`: the short form as a link to the resource's page and nothing else. Without an `href` it falls back to `short`. Never nest `IDText` inside another link.
  - `full`: the full value once; the text itself copies on click with a brief "Copied" and an `aria-label` that switches between `Copy …` and `Copied …`. There is no sibling copy button and no `cursor: copy`.
  `fallback` renders for the empty case (default `—`).
- `RelativeTime` — every timestamp: relative text with the absolute time in `title`; `fallback` renders when the value is missing.
- `TagList` — tag chips, or `—` when empty.
