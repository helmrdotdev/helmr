import { installModuleExecution } from "@helmr/module-execution"

installModuleExecution({
  root: "/opt/helmr/program",
  phase: "program",
  platformRoot: "/opt/helmr/runtime",
})
