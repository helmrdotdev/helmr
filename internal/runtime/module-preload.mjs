// runtime/typescript/src/module-preload.ts
import { installModuleExecution } from "../moduleexecution/loader.mjs";
installModuleExecution({
  root: "/opt/helmr/program",
  phase: "program",
  platformRoot: "/opt/helmr/runtime"
});
