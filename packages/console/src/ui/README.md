# Console components

The console is compact, square, flat and work-focused: 2px corners, hairline
borders, no shadows, classic gradients only on the app bar and controls, monospace for
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
- **Copy.** Sentence case everywhere: only the first word of a label, heading,
  badge, button or sentence is capitalized. Product terms are ordinary nouns
  mid-sentence ("In current deployment", "Showing 3 sessions", "No triggers
  yet."); proper nouns and acronyms keep theirs (Slack, GitHub, CLI, API, ID,
  Helmr).
- **Micro-label** (`ui.microLabel`). The one uppercase mono label: rail value
  labels, group labels and a Turn's field labels (Input, Result, Response): the
  same role gets the same style. Table column headers use it semibold in muted
  text over a strong rule (`ui.tableWrap`).
- **Empty values.** A missing value is "—" everywhere (`EmptyCell`, `KeyValue`,
  `IDText`, `RelativeTime`), never "None". Relative times are muted.
- **Focus.** Every interactive element shows the same 2px accent focus ring on
  keyboard focus; text controls show an accent border and ring instead. Every
  element carries the ring colour unfocused too, so a transition never fades it
  in from the text colour, and no component sets an outline colour of its own.
- **Current item.** Three indicators, nothing else. App bar items (section tabs
  and the Settings button) show the bordered box of their hover state
  (`ui.barItemCurrent`: strong border, panel background, full text colour). A
  vertical list (settings navigation, the phone drawer) marks its current item
  with the accent tint and a 2px accent bar on the inner left edge
  (`ui.activeBar`). Page tabs mark it with a 1px accent underline on the bottom
  edge.

## Containers

One container language for everything, in the page and floating above it:

- **In-page** containers sit in the page flow: Panel, DataTable, List, KeyValueList
  and DangerZone.
- **Floating** parts sit above the page: Popover (menus, Select, the scope pickers,
  the profile menu, the phone drawer), Dialog (every variant and the command
  palette). They are set apart by their border alone, and dialogs by the modal
  backdrop; nothing has a shadow. Auth pages (AuthWindow) have no container at
  all.

Every container has:

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
  A titled container is a region named by its title. A dialog's close control is
  a header action (`ui.closeButton`, 24px). Untitled popovers and menus have no
  header.
- **One body.** 12px padding, blocks 12px apart (`ui.containerBody`). The gap is
  the only spacing: the body's direct children drop their vertical margins.
  Dialog bodies pad 12px too. A Panel never renders an empty body: when its
  content renders nothing, only the
  header shows, and a page does not render a Panel it has nothing to put in (a
  Session without progress has no Progress panel).

Tables and lists: a DataTable or List is its own container (its own border). As a
direct child of a Panel body (`data-list-frame`, also through QueryState, which
adds no element) it has no frame: it runs flush to the Panel's edges, with a
divider only where other blocks sit above or below it, and the Panel's header
names it. Nested deeper (inside a list row, a rail value or a wrapper element) it
keeps its own frame. Steps have no frame; they sit in a Panel.

A container header never sits directly above a table's column header row:

- a page with one table shows the table itself as the container, with no Panel
  title above it (QueryState still gives it pending, error and empty states);
- a page with several named tables puts them in Tabs, one table per tab (the
  Computer page: Sessions, Commands, Secret placements, with a count where it is
  already loaded), never in stacked titled Panels;
- a list without a column header row (the Overview queue, Turns, progress,
  delivery issues, linked identities) may sit in a titled Panel.

An EmptyState that is a direct child of a container body has no dashed
border or tint; the dashed box is only for an empty state directly on the page.

There is no page-level section heading: anything titled that holds content is a
Panel.

## Shell

- **AppBar** (`components/AppBar.tsx`). The one header bar for the console,
  settings and admin areas: `leading` (scope selector or `BrandMark`),
  `nav` items, `trailing` controls. The tab's Environment draws a 2px line in its
  color along the top edge of the bar, inside its 40px height (`environmentColor`).
  Below 720px the section tabs collapse into a menu button that opens an untitled
  drawer (a `Popover` dialog with 44px `ui.drawerItem` targets); the bar keeps
  both scope pickers at phone width, so the drawer holds only the sections and
  `drawerEnd`'s Settings and account entries. Sign out is a neutral item, in the
  drawer and the profile menu.
- **NavTab.** One bar tab; it is current on its path, every path below it, and the
  paths in its `match` (a section's other sub-pages). The current tab is the
  bar's current-item box (no underline), with the count badge (14px,
  `leading-none`) inside it, clear of its edges; the tab stays 26px. The console has four
  sections: Overview, Agents (`/agents`, `/sessions`, `/triggers`), Computers
  (`/computers`, `/templates`) and Deployments. A `count` above zero shows as a
  warning badge and names the tab "Overview, 3 waiting" (the Overview's waiting
  items).
- **SectionLayout** (`components/`). A section whose pages are sub-tabs: one
  `PageHeader` and `LinkTabs` that stay mounted (a layout route) while the selected
  page renders in the tab panel. It shows the no-scope state for every page.
- **BrandMark.** The product name, optionally naming the area ("Helmr Admin");
  `bar` sets it at 15px to sit level with an `AppBar`.
- **ScopeSelector** (`components/`). The tab's scope as two segments,
  `<Project> / <Environment>` (with its dot), each opening its
  own picker: a titled `Popover`, a `CommandList` of the Projects or
  the Project's Environments (current one checked), "New project" / "New
  environment" (the shared create dialogs) and "Manage" (Project settings or
  Environments). The Project picker loads further pages while open, up to 200
  Projects, so its search covers them; past that "Load more projects" remains and
  the search says it covers loaded Projects. "Manage" is a 28px link.
- **CommandPalette** (`components/`). The header's Search button and Cmd+K on
  Apple platforms or Ctrl+K elsewhere (never a bare key, never the other
  platform's combination, never while an input method composes or another modal
  dialog is open) open a `Dialog` with a
  `CommandList`: Switch environment (the Project's Environments, then other loaded
  Projects, by Environment when their details are loaded), Go to (sections and
  Settings pages), Agents and recent Sessions (one bounded request each). Enter
  chooses, Escape closes, focus returns to where it was.
- **ProfileMenu** (`components/`). Opens in a `Popover` under a container header
  with the name and email, then the current organization (name, ID and role) with
  Switch organization…, which opens `/organizations`; then Profile, Sign-in and
  security and Sign out. The app bar never names the organization.
- **OperatorMenu** (`components/`). The admin bar's counterpart: the same
  `Popover` with the operator ID in its header, when the session ends (warning
  tone under an hour), Renew session, Operator keys and Sign out operator.
- **SettingsLayout** (`components/`). Settings pages have flat topic paths
  (`/settings/<topic>`); the navigation groups them by the level they apply to:
  Account, Project and Organization. Every settings page uses the full content
  column; no page narrows it.
- **EnvironmentSelectorLayout** and `useEditedEnvironment` (`components/`). An
  Environment-specific settings page (Secrets, API keys) edits one Environment of
  the tab's Project, chosen with one Select at the start of the page `Toolbar` at
  every width; page filters go beside it in its `tools` slot. It opens on the
  tab's Environment, is page state only (never the URL or the tab's scope), and
  starts over when the tab's scope changes. The Select tags the tab's Environment
  with the `working` Tag in its trailing column, as the Environments table does;
  while another Environment is chosen the toolbar says "You are working in X" with
  a Back to X action. The primary action names the edited Environment and
  its dialogs carry it.

## Page structure

- **Page.** Width, padding and vertical rhythm. Every route renders inside one.
- **PageHeader.** `breadcrumb` (parent pages, ending with the current page: the
  title when it is text, or `current`, a short form for long titles), `title`,
  `badge`, `subtitle` (one line, only for what the page shows nowhere else, never a
  restatement of the title), `meta` (IDs and times as separate items, never joined
  by "·"), `actions` (a list page's create action only) and `menu` (a detail
  page's object menu, see Object menus). Every route page starts with it.
  Layout: a 24px breadcrumb slot first, kept empty on pages without a breadcrumb
  so every h1 sits at the same height; then the title row (the h1, the menu
  trigger right after it, the badge), with the actions on its right; then
  subtitle and meta. The h1 is one line,
  truncated with the full title in `title`. A breadcrumb separator ends the crumb
  before it, so a wrapped line never starts with "/". On phones the actions
  follow the meta.
- **Tabs** and **TabPanel.** Sub-sections of a page with real tab roles; arrow
  keys, Home and End move between tabs. A tab is in the URL only when it is a page
  of its own; then use **LinkTabs**, whose tabs are links with the same keyboard
  behavior, inside a layout route so focus stays on the tablist (section sub-tabs,
  Agent tabs at `/agents/:agent/{triggers,slack}`, Sessions at
  `/agents/:agent`). The first page tab has no leading padding and its underline
  starts at its label, so the label lines up with the h1. A tab's count follows
  its label; the tab is named "Secret placements, 2".
- **Toolbar.** Above a list: search and filters at `start`, secondary then primary
  actions at `end`. Filters never live in the page header.
- **Panel.** The in-page container: optional `title`, `count` and `actions` in the
  container header, then the body (see Containers).
- **Object menus.** Every action on an object lives in one menu, the same
  wherever the object appears: the `RowActions` menu in its list and the chevron
  menu after its name on its detail page (`PageHeader` `menu`). Each object
  builds its items once (`agentMenu`, `groupMenu`, `roleMenu`, `memberMenu`,
  session and computer controls) and both places use them. Items appear only
  when the object's state and the account allow them; Copy link follows the
  state actions; destructive items come last, after the divider, in danger text,
  each with its `ConfirmDialog`. A detail page shows no action buttons of its
  own. Actions on one part of a page stay on that part (resume a hold in its
  notice, interrupt a turn on its row, grant access above an access table, add
  members above a members table), and create actions stay in a list's header.
- **DangerZone.** A destructive action on a part of a page that is not an
  object with its own menu (an agent's Slack connection): the danger-tone
  header, the consequence and its danger button, which opens a `ConfirmDialog`.
- **DetailLayout**, **KeyValueList**, **KeyValue.** Detail pages: main column plus a
  300px rail, 24px apart; the rail's title is its container header, then
  label/value pairs; a missing value renders "—".
- **Copy link** (`components/copyLink.tsx`). The object menu item on Session,
  Computer, Deployment and Agent menus. Project and Environment are tab state,
  never part of in-app links; it copies the one link that carries them, and
  opening it switches the tab to that scope. The full ID on a detail page copies
  on click.

## Data display

- **DataTable.** The only table. Pass `columns` (strings, or `{ label, srOnly }` for
  the actions column) and render `<tr>` rows (framing rules under Containers). Rows
  are single-line; secondary information gets its own column. Columns go in order
  of importance, the status second where there is one, so a phone shows what
  matters before it scrolls; a table that overflows fades at its right edge while
  more columns sit past it. Code in cells is body text colour; muted text is for
  explicitly secondary values. The footer is white with only a top rule.
  Conventions:
  - **PrimaryLink** — the primary cell, a mono link to the row's resource (or
    `IDText mode="link"` for IDs).
  - **EmptyCell** — "—" for a cell with no value.
  - **RowActions** — the trailing cell with one `ActionMenu`; empty when no action
    applies. Never inline button rows.
  - **RowError** — an action's error as its own row under the row it belongs to.
  - **Expanding row** — when a row's record is too large for columns (an audit
    event), a leading chevron toggle opens one row at a time; its details are a
    full-width row directly beneath it, and the open row is in the URL.
  - **LoadMore** — the `footer`: "Load more" (busy "Loading…") and the shown count.
    `noun` is the singular and plural, lower case (`{ one: "turn", other: "turns" }`):
    the count reads "Showing 1 turn" or "Showing 3 turns", the button is named
    "Load more turns". A table of two kinds of row counts both and loads the
    second (`extra`): "Showing 3 members, 1 invitation", "Load more invitations";
    the second count is left out while it is zero.
  - **TableCount** — the `footer` of a table that shows everything it has: only
    the count ("Showing 3 groups").
- **List** and **ListItem.** Non-tabular rows (turns, holds, delivery issues, linked
  identities, the Overview queue) with one divider style; framed like a DataTable;
  `footer` takes a LoadMore or a TableCount. A single-line row (`line`: the Overview queue, a
  Turn's Questions, linked identities) is 36px like a table row; a multi-block
  row (a Turn, a hold) pads 12px.
- **StatusBadge.** Every status. One tone map per resource lives in
  `StatusBadge.tsx`, with a label map where the status name does not read well
  ("In current deployment"); add a resource there instead of styling a badge
  locally. A Computer's status is **ComputerStatus** (`features/computers/`): its
  residency (cold, starting, running, parking, restoring, unavailable, with the
  unavailable reason as the tooltip and in the rail), or its lifecycle only while
  it is deleting or deleted.
- **EnvChip** and **EnvDot.** The only way an Environment is shown: its color and
  name. **EnvironmentTarget** names the Environment a dialog writes to.
- **Tag.** A short fixed fact after a name, in words: `owner`, `you`, `working`,
  `default`, `selected`. One shape everywhere; in pickers it sits in the trailing column.
  Facts are never shown as a symbol alone.
- **Icons** (`ui/Icon.tsx`). 13px stroke icons in the text colour family. An icon
  joins a word where the kind is quick to scan (a session's origin, held work,
  a recipient kind, no access); it never replaces the word. The one exception is
  the managed mark (`ManagedMark`): a lock after a definition Helmr maintains,
  explained by its title and accessible name. Text glyphs (▸ ● ↓ ✓ →) are not
  used as icons.
- **IDText** and **RelativeTime.** Every ID and timestamp (rules below).
- **CodeBlock.** Code and commands, with `copy` for things meant to be pasted.
- **Session log** (`features/sessions`). The Session page's one history view:
  a Turn timeline, Turn groups (36px headers on `ui.logTurnHeader`) and 28px event
  lines (`ui.logLine`: time, sequence, kind chip, summary, size) whose toggle spans
  the leading columns as a subgrid so the whole row is one control. Below 720px a
  line wraps to two rows. Filter chips use `ui.logChip`. Properties above it sit in
  `ui.propertyBar` cells.
- **JsonView** and **TextView** (`PayloadView`). Every payload a Session or
  question carries: Turn input, output, result and response, question
  definitions and answers. A 24px header names the kind and size and holds Copy.
  JsonView is a folding tree (root open, children closed with a one-line preview,
  Copy value and Copy path on the hovered or focused row) with a Raw toggle;
  children show in pages of 100, strings clamp at 1 KB, and a value over 64 KB
  shows the first 64 KB as raw text only. TextView clamps to 6 lines. Limits live
  in `payload.ts`.
- **Steps.** Numbered setup steps with done, current and upcoming states; a step's
  description may hold a CodeBlock. Multi-step setup is never a modal; it is Steps
  on the owning page or tab (the Agent's Slack tab, the Overview until an
  environment has agents).

## Inputs

- **Field.** Label, one control, optional `hint`, and an `error` announced as an
  alert. Controls inside take the field's label, hint and error automatically.
- **TextInput** (sans 12px), **TextArea**, **SlugInput** (mono; follows the name
  through `createDerivedSlug` until edited), **SecretInput** (write-only, never
  echoes), **SecretTextArea** (write-only and multi-line for keys and tokens: line
  breaks kept, never prefilled or shown after saving, masked where the browser
  supports it), **SearchInput**, **ColorField** (24px preset swatches, then one
  24px custom swatch that shows its color only when it is not a preset; the chosen
  swatch has the accent ring).
- **FormActions.** Cancel, then the primary action, right-aligned 8px apart. While
  submitting, Cancel is disabled and the primary action shows its busy label;
  `cancelDisabled` covers a Cancel with work of its own (signing out).

### Choosing a control

| Situation | Control |
| --- | --- |
| 2–5 mutually exclusive options that change a view | **Segmented** (an option's `count` follows its label) |
| A value in a form, more than 5 options, or long labels | **Select** (filters itself past 10 options) |
| Options that need a description | **ChoiceCards** (radio, or `multiple` for checkboxes) |
| A few options best recognized by a logo | Logo cards: a radio group of bordered cards, logo then mono name, the chosen card in the accent (the Overview's agent picker) |
| Many independent toggles grouped by scope | **CheckboxGroup** (group headings, "Select all", optional per-option description) |

There is one Select. Native `<select>` elements are not used. An option's
`trailing` tags (a Tag) sit in a trailing column and show in the
trigger for the chosen option.

## Feedback

- **QueryState.** One query's pending, error, denied, not found, empty and content
  states, so pages do not nest them by hand. The content gets the data as an
  accessor, so a refetch updates it in place. `error` is the page's sentence for a
  failed read (`loadError` in lib/error: a message per error code, else the
  fallback, never the server's text). A 403 shows that sentence as **DeniedState**;
  `notFound` replaces the error when `missing` holds (a 404 by default). Once data
  has loaded, a failed refetch keeps the content with a danger Notice and Retry
  above it; a failed next page is reported by the LoadMore footer (`failed`),
  which retries only that page. `pending` is the content's skeleton; a refetch
  never blanks loaded content.
- **EmptyState.** What is missing and the next step; may hold a CodeBlock or action.
  Dashed and tinted directly on the page; plain inside a container body.
- **NoScopeState.** The one sentence for a page without the tab's scope: "Select
  a project and environment." (or "Select a project." for project pages).
- **ErrorState.** A failed read: the sentence in danger text, a compact Retry, and
  the server's code and message under a collapsed "Technical details".
- **DeniedState.** A read the account may not make, and a page the account's
  permissions do not cover: a neutral notice naming the permission, no Retry.
  The page also hides actions that need the denied data (create buttons).
- **NotFoundState.** A resource missing from the selected Environment: what was
  asked for, where, and the way back. The page titles itself "<Resource> not
  found" and drops its status badge and actions.
- **Notice.** `info`, `warning`, `danger` or `success`; inline or `variant="bar"`
  under the app bar; `onDismiss` adds a close control. Danger is announced as an
  alert, the rest as status.
- **InlineStatus.** A short transient result next to its control ("Saved", "Copied").
- **Skeletons.** Loading shows the shape of what is coming, never a sentence or a
  bar, and only once the wait passes 250 ms (`Delayed`): **TableSkeleton** (the
  real column headers over rows of bars), **PageSkeleton** (the page's titled
  sections, optionally a rail), **LinesSkeleton** (a container body or list) and
  **SkeletonBar** (one value). Bars are static. **Spinner** is only for an action
  the person just started and is waiting on (sign-in callbacks, account linking);
  live views show a "Live" dot instead.

The dev server reaches states a local stack cannot on its own, per browser:
slow responses (`/__dev/delay`), failures and denials (`/__dev/fail`), a CLI
sign-in request (`/__dev/device`) and Slack set up in any state with delivery
issues and a linked identity (`/__dev/slack`). `dev-states.ts` lists them.

## Overlays

- **Popover.** The one floating panel: placement against its anchor, the
  container border with an optional container header, and dismissal by Escape (`useDismiss`; only
  the topmost layer closes), a press outside, Tab, or focus moving elsewhere. Tab
  leaves a menu or picker at once and a dialog-like panel past its first or last
  control. Focus returns to the anchor on every close except a press outside or
  focus moving elsewhere. Nothing is mounted while closed. ActionMenu, Select, the
  scope selector and the profile menu use it.
- **Dialog.** A modal container: the container header with the title and the
  close control, over the backdrop. Escape, the close control or a backdrop press close
  it unless `closeDisabled`. Tab cycles within it; on close, focus returns to the
  control that opened it, or to the menu trigger when it was opened from a menu
  item. Variants:
  - **FormDialog** for a short create or edit form;
  - **ConfirmDialog** for every confirmation: it names the `target` and its
    `environment`, takes a `cancelLabel`, and uses `tone="danger"` for destructive
    actions. `window.confirm` is not used;
  - **RevealDialog** for a value shown once (API key, enrollment token) with
    Copy and a warning.
- **CommandList.** A search box over a list for pickers and the command palette:
  focus stays in the box (combobox with `aria-activedescendant`), typing filters
  by every word, ArrowUp/ArrowDown move (Home/End too while the box is empty; with
  text they move the cursor), Enter chooses, items may be
  grouped and the current one is checked. Nothing reacts while an input method is
  composing.
- **ActionMenu.** The row menu: an icon-only 28px trigger (`aria-haspopup="menu"`)
  whose `aria-label` is `Actions for <target>`, opening a `menu` of `menuitem`s
  that are out of the tab order. Item labels are sentence-case verbs with no trailing
  ellipsis ("Cancel session", "Promote", "Delete"). Pass `href` for navigation and
  `tone: "danger"` for destructive items; the menu orders navigation, then ordinary
  actions, then the danger group after a separator, and supports
  ArrowUp/ArrowDown/Home/End, Enter (Space too on action buttons) and Escape.

## Auth pages

**AuthWindow** is the page for sign-in, invites, callbacks, Slack linking, CLI
device approval, access screens and onboarding: the product mark in the top-left
corner, no window or card, and one left-aligned column. `step` labels onboarding
progress above the title ("Step 1 of 2"); `account` puts **AuthAccount** (the
signed-in name and Sign out) in the top-right corner. In the column there is one
full-width primary action; secondary actions are **AuthLink** text under it, and
an error sentence sits directly above the primary. **AuthFacts** groups what the
person checks before acting (account, organization) as rows between two
hairlines; a code to compare is large mono text with no box.

Its states: the form itself, **AuthWorking** (Spinner and a message),
**AuthSuccess** (one title, one sentence naming the next step, its action),
**AuthError** (human message, folded technical details from `errorDetails`, next
step; without a title it sits inside a form, and input mistakes pass no details)
and **AuthMissing** (the link or code is missing or expired, and how to get a
new one). The raw server message is only ever in the technical details.
`AuthTitle`, `AuthCopy`, `AuthActions`, `AuthLink`, `AuthFacts` and
`AuthDivider` lay out the content.

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
  ownership and permission checks.
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
Four modes; the copy affordance exists only in `full` and `copy`:

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
- `copy`: the short form, copying the full value on click like `full`. Only where
  the full ID is already on the page in another form: a Deployment's meta, whose
  version (the title) ends with the ID.

`fallback` renders for the empty case (default "—"). `RelativeTime` renders every
timestamp: relative text with the absolute time in `title`; `fallback` renders when
the value is missing.
