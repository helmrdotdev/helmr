# Subagent Policy

Use subagents for context isolation, parallel exploration, and independent review perspectives.

## Good Uses

- Independent repository exploration.
- Review from a clear perspective such as security, correctness, or validation.
- Log or diff analysis that would pollute the main context.
- Read-only work with a bounded output.

## Poor Uses

- Small tasks where delegation overhead is larger than the work.
- Tightly coupled implementation that needs constant shared context.
- Sequential refactors where later steps depend on earlier edits.
- Unclear product or architecture decisions that need operator input.

## Output Contract

Ask subagents for concise summaries with evidence and file references. Treat subagent reports as context, not proof. The main workflow remains responsible for final triage, validation boundaries, and scope control.
