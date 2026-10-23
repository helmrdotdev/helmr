#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../.."
bun run - <<'JS'
import assert from "node:assert/strict"
import { readFileSync } from "node:fs"
import { spawnSync } from "node:child_process"

const workflow = Bun.YAML.parse(readFileSync(".github/workflows/ci.yaml", "utf8"))
assert.deepEqual([...workflow.jobs.checks.strategy.matrix.check].sort(), ["go", "policy", "typescript"])
assert.equal(workflow.concurrency["cancel-in-progress"], true)
const aggregate = workflow.jobs["ci-complete"]
assert.equal(aggregate.name, "ci complete")
assert.deepEqual([aggregate.needs].flat(), ["checks"])
const always = value => ["always()", "${{always()}}"].includes(value?.replace(/\s/g, ""))
assert.ok(always(aggregate.if), "aggregate must run after failed or skipped checks")
assert.ok(!aggregate["continue-on-error"], "aggregate failure must fail the job")
const gates = aggregate.steps.filter(step => step.env?.CHECKS_RESULT?.replace(/\s/g, "") === "${{needs.checks.result}}")
assert.equal(gates.length, 1, "aggregate must bind the checks result to one gate")
const gate = gates[0]
assert.ok(gate.if === undefined || always(gate.if), "gate must not skip failed checks")
assert.ok(!gate["continue-on-error"], "gate failure must fail the aggregate")
assert.equal(gate.shell ?? aggregate.defaults?.run?.shell ?? workflow.defaults?.run?.shell ?? "bash", "bash")
assert.equal(typeof gate.run, "string")
for (const result of ["success", "failure", "cancelled", "skipped", ""]) {
  const execution = spawnSync("bash", ["--noprofile", "--norc", "-e", "-o", "pipefail", "-c", gate.run], {
    env: { ...process.env, CHECKS_RESULT: result }, encoding: "utf8",
  })
  assert.ifError(execution.error)
  assert.equal(execution.signal, null)
  assert.equal(execution.status === 0, result === "success", `aggregate result ${JSON.stringify(result)}: ${execution.stderr}`)
  console.log(`ok - aggregate ${result || "empty"} ${execution.status === 0 ? "passes" : "fails"}`)
}
JS
printf 'ok - fixed source CI and failing aggregate\n'
