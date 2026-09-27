// runtime/typescript/src/module-preload.ts
import { installModuleLoader } from "../moduleloader/loader.mjs";
installModuleLoader({
  root: "/opt/helmr/program",
  platformRoot: "/opt/helmr/runtime"
});
