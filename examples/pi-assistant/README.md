# Analyze sales data with Pi

Give Pi a CSV, ask for a comparison, and inspect the script and report it writes.
Continue with another question using the same data and working files. This
editable, deployable Agent uses **`@earendil-works/pi-coding-agent` 1.3.0** with
one retained native session per Helmr Session.

## Data and expected result

`data/sales.csv` contains eight fictional monthly aggregates for August and
September 2026: two products across two regions. `data/README.md` describes its
schema and completeness assumptions. The Computer image copies both files to
`/workspace/data`.

For the supplied data, August totals **$3,000** and September **$2,300**, a decrease
of **$700 (23.33%)**. Atlas falls from $2,000 to $1,200; Beacon rises from $1,000 to
$1,100. These facts make the first report easy to check. They do not explain why
sales changed: the dataset contains no prices, order counts or campaign data.

## Try it

From the repository root, prepare the SDK with
`nix develop -c ./scripts/build-npm-packages.sh`. In this directory, run
`bun install --frozen-lockfile --ignore-scripts` through the repository Nix
environment. The project uses repository-local SDK packages; downloading this
folder alone does not supply those dependencies.

Create the `ANTHROPIC_API_KEY` Secret in the target Environment through your
existing Secret-management workflow. The Computer uses protected delivery for
`https://api.anthropic.com`. Never supply the value as model input or source.
The default model is `anthropic/claude-sonnet-5-5`, selected in `agents/settings.ts`.

```bash
helmr deploy PATH/TO/pi-assistant --project PROJECT --env ENVIRONMENT
helmr session create pi-assistant --text 'Compare September sales with August. Write and run a reusable analysis.mjs, then save your findings and limitations in report.md.' --project PROJECT --env ENVIRONMENT --json
```

Use the returned Session and Turn IDs, answer any questions in Console, and wait
for the Turn before inspecting its files:

```bash
helmr session turn wait TURN_ID --session SESSION_ID --timeout 5m --project PROJECT --env ENVIRONMENT --json
helmr session enqueue SESSION_ID --text 'Break the change down by product and region, and update the report with the calculations.' --project PROJECT --env ENVIRONMENT --json
```

Inspect `/workspace/analysis.mjs` and `/workspace/report.md` through the Computer
shell. Run `node analysis.mjs` in `/workspace` to reproduce the model's analysis;
compare its output with the known totals above. The Agent is instructed to show
its calculations, not only a conclusion. Generated analysis still needs review.
The same request/follow-up can be sent through the Agent's Slack connection.

To adapt this example, replace the CSV and update the data description, comparison
periods and instructions. Start a new Session after changing the deployed data;
existing Computers retain their own working files.

## Application choices

Pi's native tools can read/write files and execute commands in the Computer.
**This example does not add approval prompts for bash or file writes.** Use the
supplied synthetic data to start; choose your own tools and native extensions
when adapting it to another environment. Native extension dialogs are translated
to Helmr questions, not to a general permission system.

Native configuration, resources and tools remain editable. Live messages steer
work; Stop cancels and joins the owned native request. Completed follow-ups reuse
history and files. Reconstruction opens saved native history under
`/var/lib/helmr/pi/` and fails if that history is missing. It does not replay an
interrupted request or redo an external action.

This replaces the previous non-deployable, single-prompt Pi recipe. The example
now has a Computer definition, protected model binding, sample data and retained
Session lifecycle. It does not add scheduled reports or data-service connectors.

## Code and checks

`agents/assistant.ts` defines the image and Agent. `settings.ts` contains the model
and analysis instructions. `pi-bridge.ts`, `content.ts` and `questions.ts` match
the Pi starter. The application factory permits local paths and native options
without changing the lifecycle implementation.

Run `bun run typecheck` and `bun run test` in this directory through Nix. The real
Pi SDK executes a scripted synthetic-model response that writes/runs an analysis
script and produces a report; tests check the known totals, follow-up history,
questions, steering, cancellation, failure and reconstruction. This proves the
application plumbing, not a live model's analytical quality or VM/Slack behavior.

After preparing the compiler entries and installing both examples, run
`bun scripts/check-example-projects.ts` from the repository root. It checks
starter alignment, dependency pins, project compilation and the data-copy image
declaration. The Nix TypeScript checks run it automatically; this does not build
the Linux image or execute a paid provider.
