# Exploration Guide

Use this guide when gathering repository context before planning or implementation.

## Goal

Produce a factual map of the codebase area touched by the feature design. Exploration should reduce uncertainty for later phases without modifying files.

## What To Inspect

- Files, modules, routes, tasks, packages, or scripts directly related to the feature design.
- Nearby tests and fixtures that define expected behavior.
- Existing command, validation, formatting, and generation entrypoints.
- Language, runtime, or deployment boundaries that affect validation depth.
- Local conventions for naming, error handling, configuration, and artifacts.

## Output

Report facts discovered from the repository, not implementation guesses. Include likely implementation surface, validation surface, risks, and unknowns that planning must resolve.

Do not broaden into a full repository audit.
