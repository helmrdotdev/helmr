import { installModuleLoader } from "@helmr/module-loader"

installModuleLoader({
  root: "/opt/helmr/program",
  platformRoot: "/opt/helmr/runtime",
})
