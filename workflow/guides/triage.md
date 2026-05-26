# Triage Guide

Use this guide when converting one or more reviews into a fix list.

## Keep

Keep a finding only when it is a real blocker before PR creation:

- It identifies a concrete failure mode.
- It names an affected file, behavior, or contract.
- It explains how the current diff can trigger the issue.
- The fix is bounded enough for the implementation agent.

## Drop

Drop false positives, duplicates, style preferences, missing ideal tests, speculative risks, and requests without evidence from the diff or repository contract.

## Validation Findings

Use implementation and fix reports to avoid stale validation requests. Keep a validation finding only when validation is missing, failed, irrelevant, or insufficient for the changed behavior. Direct non-Nix development commands are insufficient unless they were run inside a Nix shell.

Prefer fewer, higher-confidence findings over broad defensive lists.
