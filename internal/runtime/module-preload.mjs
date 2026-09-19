// runtime/typescript/src/module-preload.ts
import { installModuleExecution } from "../moduleexecution/loader.mjs";
installModuleExecution({
  root: "/opt/helmr/program",
  platformRoot: "/opt/helmr/runtime"
});
