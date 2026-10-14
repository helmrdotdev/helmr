# Console components

The console is compact, square and work-focused: 2px corners, hairline borders,
classic gradient bars, title strips and hard 2px offset shadows on floating parts, monospace for
navigation, labels, IDs, code and values, and Inter for everything else. Build pages
from the parts below instead of page-local class strings. When a repeated style
carries structure or behavior, it is a component here; when it is a single element
style, it is a `ui` token in `styles.ts`. Avoid broad descendant overrides that
silently change nested controls.

## Foundations

- **Tokens** (`src/styles.css`). Semantic colors only: `console-bg`, `bg-panel`,
  `surface`, `border`, `border-strong`, `text`, `muted`, `subtle`, `faint`,
  `accent`, `focus`. Muted and subtle meet WCAG AA on every surface; faint is
  decorative (empty-value dashes, placeholders). Each tone (`info`, `warning`,
  `success`, `danger`, `neutral`) is a triple of text, `-bg` and `-border`;
  `toneClass[tone]` applies all three. No hex colors in pages; Environment colors
  are data and are applied inline.
- **Type set.** `text-body` 12.5px, `text-control` 12px (form values), `text-chrome`
  11.5px (mono navigation, buttons, IDs), `text-badge` 11px, `text-micro` 10.5px.
  Page and section headings use `ui.h1` and `ui.h2`, container headers
  `ui.containerTitle`. Nothing else.
- **Sizes.** Controls are 28px high (`h-7`), bar tabs 26px, page tabs 36px, table
  rows 36px with a 32px header, container headers 32px. Phone drawer targets are
  44px. The one smaller button is `ui.compactButton` (24px, secondary): container
  header actions, table and list footers, notice actions, a code block's Copy and
  a queue row's action.
- **Spacing.** 4/8/12/16/24/32. Buttons in a group sit 8px apart (`ui.buttonGroup`),
  page sections stack 24px apart (`ui.sectionStack`), blocks inside a container,
  dialog or list row sit 12px apart (`ui.blockStack`).
- **Micro-label** (`ui.microLabel`). The one uppercase mono label: table column
  headers, rail value labels, group labels.
- **Focus.** Every interactive element shows the same 2px focus ring on keyboard
  focus; text controls show an accent border and ring instead.

## Containers

Two kinds, never mixed:

- **Floating** parts sit above the page: Popover (menus, pickers), Dialog (every
  variant and the command palette) and AuthWindow. Only they use the window
  chrome: a bordered surface with the hard shadow (`ui.window`, `ui.popover`,
  `shadow-console-window`) and the 22px gradient title strip (`ui.titleStrip`).
- **In-page** containers sit in the page flow: Panel, DataTable, List, KeyValueList
  and DangerZone. They have no shadow and no title strip.

Every in-page container has:

- **One border.** `ui.container`: `border-console-border-strong` on
  `bg-console-surface`. Semantic exceptions keep their distinction: EmptyState is
  dashed, DangerZone uses the danger border, CodeBlock keeps its panel background.
- **One header**, only when it has a title (`ContainerHeader`, `ui.containerHeader`):
  32px like a table's column header row, flat `bg-console-bg-panel`, bottom border,
  12px horizontal padding; the title in sans 12.5px medium text colour, sentence
  case (`ui.containerTitle`); an optional count in mono subtle
  (`ui.containerCount`); actions on the right as compact buttons or short mono
  status text. A description is never in the header; it is the body's first line.
  DangerZone uses the same header in the danger tone (`ui.containerHeaderDanger`).
  A titled container is a region named by its title.
- **One body.** 12px padding, blocks 12px apart (`ui.containerBody`). The gap is
  the only spacing: the body's direct children drop their vertical margins.

Tables and lists: a DataTable or List is its own container (its own border). As a
direct child of a Panel body (`data-list-frame`, also through QueryState, which
adds no element) it has no frame: it runs flush to the Panel's edges, with a
divider only where other blocks sit above or below it, and the Panel's header
names it. Nested deeper (inside a list row, a rail value or a wrapper element) it
keeps its own frame. A table or list that needs a title sits in a Panel. Steps have
no frame; they sit in a Panel.

A page `Section` heading sits outside containers and never repeats the title of
the container right under it.

## Shell

- **AppBar** (`components/AppBar.tsx`). The one header bar for the console,
  settings and admin areas: `leading` (scope selector or `BrandMark`),
  `nav` items, `trailing` controls. A protected Environment draws a 2px line in its
  color along the top edge of the bar, inside its 40px height (`protectedColor`).
  Below 720px the section tabs collapse into a menu button that opens a drawer
  (a `Popover` dialog with 44px `ui.drawerItem` targets); `drawerStart` and
  `drawerEnd` add the scope and the Settings and account entries.
- **NavTab.** One bar tab; it is current on its path, every path below it, and the
  paths in its `match` (a section's other sub-pages). The console has four
  sections: Overview, Agents (`/agents`, `/sessions`, `/triggers`), Computers
  (`/computers`, `/templates`) and Deployments. A `count` above zero shows as a
  warning badge and names the tab "Overview, 3 waiting" (the Overview's waiting
  items).
- **SectionLayout** (`components/`). A section whose pages are sub-tabs: one
  `PageHeader` and `LinkTabs` that stay mounted (a layout route) while the selected
  page renders in the tab panel. It shows the no-scope state for every page.
- **BrandMark.** The product name, optionally naming the area ("Helmr Admin").
- **ScopeSelector** (`components/`). The tab's scope as two segments,
  `<Project> / <Environment>` (with its dot and "protected" tag), each opening its
  own picker: a `Popover` with a title strip, a `CommandList` of the Projects or
  the Project's Environments (current one checked), "New project" / "New
  environment" (the shared create dialogs) and "Manage" (Project settings or
  Environments). The Project picker loads further pages while open, up to 200
  Projects, so its search covers them; past that "Load more projects" remains and
  the search says it covers loaded Projects. The phone menu's "Change project" and
  "Change environment" open the same pickers.
- **CommandPalette** (`components/`). The header's Search button and Cmd+K on
  Apple platforms or Ctrl+K elsewhere (never a bare key, never the other
  platform's combination, never while an input method composes or another modal
  dialog is open) open a `Dialog` with a
  `CommandList`: Switch environment (the Project's Environments, then other loaded
  Projects, by Environment when their details are loaded), Go to (sections and
  Settings pages), Agents and recent Sessions (one bounded request each). Enter
  chooses, Escape closes, focus returns to where it was.
- **ProfileMenu** (`components/`). Opens in a `Popover`.
- **SettingsLayout** (`components/`). Settings pages have flat topic paths
  (`/settings/<topic>`); the navigation groups them by the level they apply to:
  Account, Project and Organization.
- **EnvironmentSelectorLayout** and `useEditedEnvironment` (`components/`). An
  Environment-specific settings page (Secrets, API keys) edits one Environment of
  the tab's Project, chosen in a rail (a Select on narrow screens). It opens on the
  tab's Environment, is page state only (never the URL or the tab's scope), and
  starts over when the tab's scope changes. The rail tags the tab's Environment
  "working" (the narrow Select adds "protected" and "working" to option text). The
  page description shows the edited
  Environment with `EditedEnvironmentChip`, the primary action names it, and its
  dialogs carry it.

## Page structure

- **Page.** Width, padding and vertical rhythm. Every route renders inside one.
- **PageHeader.** `breadcrumb` (parent pages), `title`, `badge`, `subtitle` (one
  line), `meta` (IDs and times), `actions` (Copy link first, the primary action
  last). Every route page starts with it.
- **Tabs** and **TabPanel.** Sub-sections of a page with real tab roles; arrow
  keys, Home and End move between tabs. A tab is in the URL only when it is a page
  of its own; then use **LinkTabs**, whose tabs are links with the same keyboard
  behavior, inside a layout route so focus stays on the tablist (section sub-tabs,
  Agent tabs at `/agents/:agent/{triggers,slack,definition}`, Sessions at
  `/agents/:agent`).
- **Toolbar.** Above a list: search and filters at `start`, secondary then primary
  actions at `end`. Filters never live in the page header.
- **Section.** Page-level heading with optional count, description and actions,
  then content (see Containers).
- **Panel.** The in-page container: optional `title`, `count` and `actions` in the
  container header, then the body (see Containers).
- **DangerZone.** A destructive page action: the danger-tone header, the
  consequence and one danger button that opens a `ConfirmDialog`. Last in the rail
  or on the page.
- **DetailLayout**, **KeyValueList**, **KeyValue.** Detail pages: main column plus a
  300px rail; the rail's title is its container header, then label/value pairs; a
  missing value renders "—".
- **CopyLinkButton** (`components/`). The first `PageHeader` action on Session,
  Computer, Deployment and Agent detail pages. Project and Environment are tab
  state, never part of in-app links; this button copies the one link that carries
  them, and opening it switches the tab to that scope.

## Data display

- **DataTable.** The only table. Pass `columns` (strings, or `{ label, srOnly }` for
  the actions column) and render `<tr>` rows (framing rules under Containers). Rows
  are single-line; secondary information gets its own column. Conventions:
  - **PrimaryLink** — the primary cell, a mono link to the row's resource (or
    `IDText mode="link"` for IDs).
  - **EmptyCell** — "—" for a cell with no value.
  - **RowActions** — the trailing cell with one `ActionMenu`; empty when no action
    applies. Never inline button rows.
  - **RowError** — an action's error as its own row under the row it belongs to.
  - **LoadMore** — the `footer`: "Load more" (busy "Loading…") and the shown count.
    `noun` names the button ("Load more Sessions") and the count.
- **List** and **ListItem.** Non-tabular rows (turns, holds, delivery issues, linked
  identities, the Overview queue) with one divider style and 12px padding; framed
  like a DataTable; `footer` takes a LoadMore.
- **StatusBadge.** Every status. One tone map per resource lives in
  `StatusBadge.tsx`, with a label map where the status name does not read well
  ("In current Deployment"); add a resource there instead of styling a badge
  locally.
- **EnvChip** and **EnvDot.** The only way an Environment is shown: its color, name
  and "protected" tag. **EnvironmentTarget** names the Environment a dialog writes to.
- **IDText** and **RelativeTime.** Every ID and timestamp (rules below).
- **CodeBlock.** Every block of code or output, with `copy` for things meant to be
  pasted.
- **Steps.** Numbered setup steps with done, current and upcoming states; a step's
  description may hold a CodeBlock. Multi-step setup is never a modal; it is Steps
  on the owning page or tab (the Agent's Slack tab, the Overview before the first
  Deployment).

## Inputs

- **Field.** Label, one control, optional `hint`, and an `error` announced as an
  alert. Controls inside take the field's label, hint and error automatically.
- **TextInput** (sans 12px), **TextArea**, **SlugInput** (mono; follows the name
  through `createDerivedSlug` until edited), **SecretInput** (write-only, never
  echoes), **SecretTextArea** (write-only and multi-line for keys and tokens: line
  breaks kept, never prefilled or shown after saving, masked where the browser
  supports it), **SearchInput**, **ColorField** (custom color plus preset swatches).
- **FormActions.** Cancel, then the primary action, right-aligned 8px apart. While
  submitting, Cancel is disabled and the primary action shows its busy label;
  `cancelDisabled` covers a Cancel with work of its own (signing out).

### Choosing a control

| Situation | Control |
| --- | --- |
| 2–5 mutually exclusive options that change a view | **Segmented** (an option's `count` follows its label) |
| A value in a form, more than 5 options, or long labels | **Select** (filters itself past 10 options) |
| Options that need a description | **ChoiceCards** (radio, or `multiple` for checkboxes) |
| Many independent toggles grouped by scope | **CheckboxGroup** (group headings, "Select all", optional per-option description) |

There is one Select. Native `<select>` elements are not used.

## Feedback

- **QueryState.** One query's pending, error (with Retry), empty (title, hint,
  action) and content states, so pages do not nest them by hand. The content gets
  the data as an accessor, so a refetch updates it in place. Once data has loaded,
  a failed refetch keeps the content with a danger Notice and Retry above it; a
  failed next page is reported by the LoadMore footer (`failed`), which retries
  only that page. Without `pending`, nothing renders during the first load.
- **EmptyState.** What is missing and the next step; may hold a CodeBlock or action.
  Also the state for something a page computes itself, such as no selected scope.
- **ErrorState.** A failure with Retry; QueryState uses it.
- **Notice.** `info`, `warning`, `danger` or `success`; inline or `variant="bar"`
  under the app bar; `onDismiss` adds a close control. Danger is announced as an
  alert, the rest as status.
- **InlineStatus.** A short transient result next to its control ("Saved", "Copied").
- **Progress.** The striped bar for redirects and long operations.

## Overlays

- **Popover.** The one floating panel: placement against its anchor, window
  chrome with an optional title strip, and dismissal by Escape (`useDismiss`; only
  the topmost layer closes), a press outside, Tab, or focus moving elsewhere. Tab
  leaves a menu or picker at once and a dialog-like panel past its first or last
  control. Focus returns to the anchor on every close except a press outside or
  focus moving elsewhere. Nothing is mounted while closed. ActionMenu, Select, the
  scope selector and the profile menu use it.
- **Dialog.** A modal window. Escape, the close control or a backdrop press close
  it unless `closeDisabled`. Tab cycles within it; on close, focus returns to the
  control that opened it, or to the menu trigger when it was opened from a menu
  item. Variants:
  - **FormDialog** for a short create or edit form;
  - **ConfirmDialog** for every confirmation: it names the `target` and its
    `environment`, takes a `cancelLabel`, and uses `tone="danger"` for destructive
    actions. `window.confirm` is not used;
  - **RevealDialog** for a value shown once (API key, invite link, enrollment
    token) with Copy and a warning.
- **CommandList.** A search box over a list for pickers and the command palette:
  focus stays in the box (combobox with `aria-activedescendant`), typing filters
  by every word, ArrowUp/ArrowDown move (Home/End too while the box is empty; with
  text they move the cursor), Enter chooses, items may be
  grouped and the current one is checked. Nothing reacts while an input method is
  composing.
- **ActionMenu.** The row menu: an icon-only 28px trigger whose `aria-label` is
  `Actions for <target>`. Item labels are sentence-case verbs with no trailing
  ellipsis ("Cancel Session", "Promote", "Delete"). Pass `href` for navigation and
  `tone: "danger"` for destructive items; the menu orders navigation, then ordinary
  actions, then the danger group after a separator, and supports
  ArrowUp/ArrowDown/Home/End, Enter/Space and Escape.

## Auth windows

**AuthWindow** is the centered window for sign-in, invites, callbacks, Slack
linking, CLI device approval, access screens and onboarding (`step` shows progress
in the title strip, with **Steps** in the window). Its states: the form itself,
**AuthWorking** (Progress and a message), **AuthSuccess** (result, optional redirect
bar), **AuthError** (human message, folded technical details from `errorDetails`,
next step; without a title it sits inside a form) and **AuthMissing** (the link or
code is missing or expired, and how to get a new one). The raw server message is
only ever in the technical details. `AuthTitle`, `AuthCopy`,
`AuthActions` and `AuthDivider` lay out the content.

## Shared behavior

- `useDismiss` (`ui/useDismiss.ts`) — Escape and outside-press dismissal with a
  stack, so Escape closes only the topmost layer.
- `useClipboard` (`ui/useClipboard.tsx`) — copy with one feedback timer and a
  polite live region; every copy control uses it.
- `slugify` (`lib/slug.ts`) — identifiers from display names.
- `errorMessage` (`lib/error.ts`) — the message for a failed request: a
  page-specific message for the error code, then the server message, then the
  fallback. `authErrorMessage` maps sign-in failure codes; `errorDetails` is the
  code and server message for folded technical details.
- `useMe`, `usePermission` (`lib/me.ts`) — the one account query and the
  owner/admin and permission checks.
- `createRowActions` (`ui/rowActions.ts`) — busy and error state for per-row actions.

## IDs and actions

This table sets where IDs and actions live. Apply it to every page instead of
choosing per page.

| Place | IDs | Actions |
| --- | --- | --- |
| List rows | Prefer a human-readable identifier (Agent name, key, version). Keep an ID column only where nothing better exists (Sessions, Computers), rendered with `IDText` short form: last hyphen segment of the UUID, mono muted, full ID in `title`. No copy control. | None inline. Rows navigate to the detail page: the primary cell is the link (`IDText mode="link"` or `PrimaryLink` on the readable identifier). Rarely used actions go into `RowActions` in a trailing `{ label: "Actions", srOnly: true }` column. Never render a menu whose only items duplicate the primary cell's navigation. |
| Detail pages | The full ID once, in the `PageHeader` `meta` or `subtitle`, with `IDText mode="full"` (click-to-copy). Rail entries for related resources use `IDText mode="link"`, so long UUIDs never wrap. | Page-wide actions in the `PageHeader` `actions` slot; actions on individual holds stay with that hold, confirmed with `ConfirmDialog` or a `FormDialog`. |
| Overview queue | Agent name and Session short form (`IDText mode="link"`) as links. | One primary action per item (Answer, Review, Inspect, Reauthorize) leading to where it is done. The Console never answers Questions; Answer opens the Question page with its Slack and CLI guidance. |

`IDText` renders every ID and digest: monospace, muted, full value in `title`.
Three modes; the copy affordance exists only in `full`:

- `short` (default): the short form as plain text. `formatID` in `ui/id.ts` is the
  shared rule: the last hyphen segment of a UUID (12 hex characters), `algo:` plus
  the last 12 characters of a digest, no leading ellipsis; any other value is shown
  unchanged.
- `link` with `href`: the short form as a link to the resource's page and nothing
  else. Without an `href` it falls back to `short`. Never nest `IDText` inside
  another link.
- `full`: the full value once; the text itself copies on click with a brief
  "Copied" and an `aria-label` that switches between `Copy …` and `Copied …`. There
  is no sibling copy button and no `cursor: copy`.

`fallback` renders for the empty case (default "—"). `RelativeTime` renders every
timestamp: relative text with the absolute time in `title`; `fallback` renders when
the value is missing.
