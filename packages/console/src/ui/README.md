# Console UI conventions

The console UI is intentionally compact, square, and work-focused.

- Prefer existing primitives in `src/ui` before adding page-local class strings.
- Keep form controls aligned at `28px` high: `ui.button`, `ui.secondaryButton`, `ui.input`, and `ui.selectTrigger`.
- Use `AuthScreen`, `AuthTitle`, `AuthCopy`, `AuthDivider`, and `AuthActions` for auth/device/callback screens.
- Use `Modal` for dialogs so outside click, Escape, close button, sizing, and header treatment stay consistent.
- Use `ConfirmModal` for cancel, promote, and other confirmations so the button order, busy state, and error placement stay the same.
- Use `ui.actionRow` for ordinary action rows and `ui.modalActions` only inside modals.
- Use `ui.field`, `ui.fieldError`, `ui.fieldSet`, and `ui.fieldLegend` for forms instead of page-local label and legend classes.
- Use semantic Tailwind tokens from `styles.css` for new one-off layouts, for example `bg-console-surface`, `border-console-border`, `text-console-muted`, `font-console-mono`.
- Keep arbitrary values for exact UI details that are part of the console visual language, such as `rounded-xs` or fixed table heights.

When a repeated style includes structure or behavior, make a Solid component. When it is a single repeated element style, add a `ui` token. Avoid broad descendant overrides that silently change nested controls.

## Page and section structure

- `PageHeader` — title, optional badge, subtitle, back link, and right-aligned actions. Every route page starts with it.
- `SectionHeader` — an `h2` with optional count, subtitle, and "view all" style actions for sections inside a page.
- `Panel`, `DetailList`, `DetailItem` — detail pages: a bordered panel with a heading, and the right-hand rail of label/value pairs.
- `StatePanel` — the only loading, empty, and error rendering: `<StatePanel loading="…" />`, `<StatePanel error="…" />`, `<StatePanel empty="…" hint="…">optional action</StatePanel>`.

## Tables

- `DataTable` — the only table wrapper. Pass `columns` (strings, or `{ label, srOnly }` for action columns) and render `<tr>` rows as children. Header and cell styling live in `ui.tableWrap`; pass `minWidth` (a Tailwind `min-w-*` class) when the table needs horizontal scroll on narrow screens.
- Rows are single-line. Secondary information (kind, tags, IDs, failure messages) gets its own column instead of a second line inside a cell. There is no two-line cell primitive; if a cell would wrap, add a column.
- `StatusBadge` — `<StatusBadge resource="run" status={run.status} />`. One tone map per resource lives in `StatusBadge.tsx`; add a resource there instead of styling a badge locally. Labels are derived from the status value.
- `IDText` — every ID, digest, and key: monospace, shows the tail (`…` + last 12 characters) so IDs that share a time-based prefix stay distinguishable, full value in `title`, click copies. Pass `full` in detail rails.
- `RelativeTime` — every timestamp: relative text with the absolute time in `title`; `fallback` renders when the value is missing.
- `TagList` — tag chips, or `—` when empty.
