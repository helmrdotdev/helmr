# Console UI conventions

The console UI is intentionally compact, square, and work-focused.

- Prefer existing primitives in `src/ui` before adding page-local class strings.
- Keep form controls aligned at `28px` high: `ui.button`, `ui.secondaryButton`, `ui.input`, and `ui.selectTrigger`.
- Use `AuthScreen`, `AuthTitle`, `AuthCopy`, `AuthDivider`, and `AuthActions` for auth/device/callback screens.
- Use `Modal` for dialogs so outside click, Escape, close button, sizing, and header treatment stay consistent.
- Use `ui.actionRow` for ordinary action rows and `ui.modalActions` only inside modals.
- Use `ui.field`, `ui.fieldError`, `ui.fieldSet`, and `ui.fieldLegend` for forms instead of page-local label and legend classes.
- Use semantic Tailwind tokens from `styles.css` for new one-off layouts, for example `bg-console-surface`, `border-console-border`, `text-console-muted`, `font-console-mono`.
- Keep arbitrary values for exact UI details that are part of the console visual language, such as `rounded-xs` or fixed table heights.

When a repeated style includes structure or behavior, make a Solid component. When it is a single repeated element style, add a `ui` token. Avoid broad descendant overrides that silently change nested controls.
