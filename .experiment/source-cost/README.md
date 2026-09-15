# PR309 source-cost experiment

Temporary review/CI mirror; remove this directory and its PR309-only workflow
steps before merging. No file here is imported by production. It makes a scratch
instrumented copy of the generated adapter and compares incumbent, observation
and frozen-emit reuse using the same TypeScript API and native source identities.
This is not a production cache or admission contract.

Run through the pinned development shell:

```sh
nix develop --command .experiment/source-cost/run.sh /tmp/source-cost-fresh
```

Docker uses the executor's native architecture and exact Node24.21.0; each measured
process is network-disabled with read-only source/platform mounts. Installation
preparation fetches the locked public OpenCode SDK with scripts disabled and uses
the locally packed Helmr SDK. No Factory/private files or handlers are included.
Record capture occurs during the normal graph-validation import; it does not scan
or transform dormant malformed TypeScript. Reuse validates the frozen cache digest
and records keyed by source, canonical filename, exact effective emit options and
platform byte identities. Misses use the incumbent transformer.

Outputs include raw interleaved fresh-process samples, native warm import samples,
API/adapter/config/emit/key timings, RSS, file I/O counters, cache bytes and source
inventories. The observation-minus-incumbent delta exposes instrumentation cost.
Keep eager TypeScript in every adapter arm. Do not interpret warm ESM imports as
transform-cache savings, or infer an SLO/KVM result from these measurements. Whole
Go artifact admission is outside this mirror; the cache remains derived scratch.
