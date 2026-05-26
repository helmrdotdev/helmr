# Planning Guide

Use this guide for implementation planning and plan review.

## Planning Principles

- Convert the feature design and exploration report into small executable steps.
- Preserve repository patterns found during exploration.
- Identify the validation boundary before implementation starts.
- Prefer direct changes to new abstractions unless the repository already has a matching pattern.
- Keep out-of-scope files and behaviors explicit.

## Validation Boundary

Classify the change before choosing checks:

- Local TypeScript or workflow logic.
- Go control-plane business logic.
- CLI or API behavior.
- Database schema or migrations.
- Worker, guestd, sandbox, image mode, Firecracker, filesystem mounts, or run execution.
- UI or browser behavior.
- AWS infrastructure or deployment path.

Use the smallest validation layer that can catch the likely regression, but state when VM worker, browser, AWS dev stack, or operator validation remains necessary.

## Plan Review

Plan review should block only when the plan has missing scope, missing validation, risky assumptions, or integration mistakes likely to cause implementation failure.
