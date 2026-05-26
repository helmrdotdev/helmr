# Review Guide

Use this guide for code review phases.

## Review Bar

Report only actionable issues that should block PR creation or require a fix before merge. Each finding must have:

- A concrete failure mode.
- The affected file, function, or behavior when known.
- Evidence from the diff or a repository contract.
- A concrete fix.

## Priorities

1. Correctness, data loss, security, auth, secret handling, and permissions.
2. Contract and API compatibility, migrations, concurrency, retries, and error handling.
3. Missing or weak validation for behavior touched by the change.
4. Maintainability issues likely to cause future defects.

Ignore style preferences, broad cleanup requests, and speculative improvements unless they affect correctness or operability.

## Validation

Do not trust implementation reports blindly. Compare claimed validation with the diff and repository contracts. Direct non-Nix development commands are a validation gap unless the report clearly says they ran inside a Nix shell.
