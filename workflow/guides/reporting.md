# Reporting Guide

Use this guide for implementation and fix reports.

## Required Final Report

Include these sections:

- Summary: what changed.
- Changed files: exact repo-relative paths.
- Validation ledger: for each command include working directory, exact command, exit status, why it was relevant, and result summary.
- Scope audit: state that `git status --short` and `git diff --stat` were reviewed; unrelated changes are `none`, `reverted`, or explicitly explained.
- Gaps or blockers: only real remaining issues, or `none`.

## Validation Ledger

Validation must be specific enough for reviewers to judge coverage. Do not say "tests passed" without naming the command and working directory.

If a relevant check could not run, report what was attempted, why it failed or was skipped, and what behavior remains unverified.

## Scope Audit

The report should make it easy to distinguish intentional implementation files from accidental edits, generated files, local artifacts, and unrelated cleanup.
