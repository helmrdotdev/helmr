#!/usr/bin/env bash
# Reads BuildKit --progress=plain logs of a helmr build. BuildKit names a step
# "#N [stage i/n] ..." and reports reuse as "#N CACHED". Step numbers restart for
# every graph of a build, so a marker is looked up inside the graph that
# contains the step.
#
# step_cached LOG STEP: 0 when the step was reused, 1 when it ran, 2 when the
# log does not contain the step at all (the assertion itself is wrong).
step_cached() {
  local log="$1" step="$2" numbered graph id
  numbered=$(awk '/^#0 building with /{ graph++ } { print graph "\t" $0 }' "$log")
  graph=$(grep -F -- "$step" <<<"$numbered" | head -n 1 | cut -f1)
  [ -n "$graph" ] || { echo "step not found in $log: $step" >&2; return 2; }
  id=$(grep -F -- "$step" <<<"$numbered" | head -n 1 | cut -f2 | cut -d' ' -f1)
  if grep -Fx -- "$graph"$'\t'"$id CACHED" <<<"$numbered" >/dev/null; then
    return 0
  fi
  return 1
}

# step_ran LOG STEP: succeeds only when the step is present and was not reused.
# A missing step is an error, never evidence that something reran.
step_ran() {
  local status=0
  step_cached "$1" "$2" || status=$?
  case "$status" in
    1) return 0 ;;
    0) echo "step was reused in $1: $2" >&2; return 1 ;;
    *) return "$status" ;;
  esac
}
