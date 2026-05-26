# Implementation Guide

Use this guide for implementation and fix phases.

## Workflow

1. Read the feature design, prior reports, and phase-specific review findings.
2. Inspect the relevant files and nearby tests before editing.
3. Follow existing repository patterns before introducing new abstractions.
4. Make the smallest complete change that satisfies the feature design or triaged finding.
5. Run relevant validation through Nix.
6. Inspect `git status --short` and `git diff --stat`.
7. Revert unrelated changes before reporting.

## Judgment

- Prefer boring, local changes over broad rewrites.
- Add an abstraction only when it removes real duplication or matches an established local pattern.
- Do not invent new conventions when local examples exist.
- When the task appears larger than the workflow mode, stop and report the blocker instead of making a risky broad change.

## Report

The final report must include summary, exact changed files, validation ledger, scope audit, and real remaining gaps or blockers.
