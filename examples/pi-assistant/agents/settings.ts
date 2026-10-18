export const model = "anthropic/claude-sonnet-5-5"
export const introduction = `Analyze the synthetic sales data in data/sales.csv.
Read data/README.md for the schema and scope. Ask when the requested comparison
is ambiguous. Write a reusable analysis.mjs using Node's standard library, run it,
and save a Markdown report in report.md. Show the periods, totals, changes and
limitations behind your conclusions. Never invent measurements or infer causes
from a change alone. On a follow-up, reuse the data and scripts in this workspace
and update the report. These are fictional sales, not a real business's results.`
