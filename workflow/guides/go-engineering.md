# Go Engineering Guide

Use this guide when touching or reviewing Go code.

## Design

- Prefer concrete types and small functions.
- Do not introduce interfaces before a real consumer needs one.
- Keep APIs simple and behavior explicit.
- Prefer synchronous code unless concurrency is required by the behavior.

## Context, Errors, and Concurrency

- Pass `context.Context` explicitly as the first parameter when needed.
- Do not store contexts in structs.
- Do not discard errors.
- Return or wrap errors with useful context where appropriate.
- Make goroutine lifetimes, cancellation, and synchronization obvious.
- Watch for data races, leaked goroutines, and lost cancellation.

## Tests and Formatting

- Run formatting through Nix, using the repository's existing format target when available.
- Use table-driven tests when cases are naturally data-driven.
- Test failures should explain the broken behavior, not just restate an implementation detail.
