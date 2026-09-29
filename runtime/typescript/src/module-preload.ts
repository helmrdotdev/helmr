import { installModulePolicy } from "./module-policy"

installModulePolicy({
  root: "/opt/helmr/program",
  platformEntries: ["/opt/helmr/runtime/helmr/entry.mjs"],
})
