// packages/module-execution/src/index.ts
import { createHash as createHash2 } from "node:crypto";
import { existsSync as existsSync2, readFileSync as readFileSync2, statSync as statSync2 } from "node:fs";
import { isBuiltin, registerHooks } from "node:module";
import { dirname as dirname2, extname, join as join2, resolve as resolve2 } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import ts2 from "./typescript.cjs";

// packages/module-execution/src/authority.ts
import { createHash } from "node:crypto";
import { existsSync, lstatSync, readFileSync, realpathSync, statSync } from "node:fs";
import { dirname, isAbsolute, join, relative, resolve } from "node:path";
import ts from "./typescript.cjs";
function isContained(root, path) {
  const value = relative(root, path);
  return value !== ".." && !value.startsWith(`../`) && !isAbsolute(value);
}
var SourceAuthority = class {
  root;
  configReads = /* @__PURE__ */ new Map();
  options = /* @__PURE__ */ new Map();
  constructor(root) {
    this.root = realpathSync(root);
  }
  // Check the existing prefix too: a missing file below an escaping symlink
  // must not turn an authority error into alias/extension fallback.
  contained(path) {
    path = resolve(path);
    if (!isContained(this.root, path)) this.escape(path);
    let prefix = path;
    while (!existsSync(prefix)) {
      try {
        if (lstatSync(prefix).isSymbolicLink()) this.escape(prefix);
      } catch (error) {
        if (!missing(error)) throw error;
      }
      const parent = dirname(prefix);
      if (parent === prefix) break;
      prefix = parent;
    }
    if (!isContained(this.root, realpathSync(prefix))) this.escape(path);
    return path;
  }
  file(path) {
    return realpathSync(this.contained(path));
  }
  escape(path) {
    throw new Error(`Helmr module source must stay inside Program ${this.root}: ${path}`);
  }
  assertEntry() {
    let value;
    try {
      const manifest = this.file(join(this.root, "package.json"));
      if (!statSync(manifest).isFile()) throw new Error("not a regular file");
      value = JSON.parse(readFileSync(manifest, "utf8"));
    } catch (cause) {
      throw new Error("Helmr Program requires a contained regular root package.json", { cause });
    }
    if (value === null || typeof value !== "object" || Array.isArray(value)) {
      throw new Error("Helmr Program root package.json must contain a JSON object");
    }
    for (let directory = dirname(this.root); ; directory = dirname(directory)) {
      const candidate = join(directory, "node_modules");
      try {
        lstatSync(candidate);
        throw new Error(`Helmr managed module root has an ancestor node_modules search location: ${candidate}; use an image/project layout without this ancestor entry`);
      } catch (error) {
        if (!missing(error)) throw error;
      }
      if (dirname(directory) === directory) break;
    }
    if (process.env["NODE_PATH"] || process.env["NODE_OPTIONS"]) {
      throw new Error("Helmr managed modules require NODE_PATH and NODE_OPTIONS to be unset");
    }
    if (!process.execArgv.includes("--no-global-search-paths")) {
      throw new Error("Helmr managed modules require Node --no-global-search-paths");
    }
  }
  format(path) {
    if (/\.(?:mjs|mts)$/.test(path)) return "module";
    if (/\.(?:cjs|cts)$/.test(path)) return "commonjs";
    for (let directory = dirname(path); isContained(this.root, directory); directory = dirname(directory)) {
      if (directory.endsWith("/node_modules")) break;
      const manifest = join(directory, "package.json");
      this.contained(manifest);
      if (existsSync(manifest)) {
        const value = JSON.parse(readFileSync(this.file(manifest), "utf8"));
        return value?.type === "module" ? "module" : "commonjs";
      }
      if (directory === this.root) break;
    }
    return "commonjs";
  }
  config(path) {
    let config;
    for (let directory = dirname(this.file(path)); ; directory = dirname(directory)) {
      const candidate = this.contained(join(directory, "tsconfig.json"));
      if (existsSync(candidate) && statSync(candidate).isFile()) {
        config = candidate;
        break;
      }
      if (directory === this.root) return {};
    }
    const cached = this.options.get(config);
    if (cached !== void 0) return cached;
    const allowed = (path2) => isContained(this.root, resolve(path2)) ? this.contained(path2) : void 0;
    const host = {
      useCaseSensitiveFileNames: true,
      getCurrentDirectory: () => this.root,
      readDirectory: () => [],
      onUnRecoverableConfigFileDiagnostic: (diagnostic) => {
        throw diagnosticError([diagnostic]);
      },
      fileExists: (path2) => {
        const value = allowed(path2);
        return value !== void 0 && existsSync(value) && statSync(value).isFile();
      },
      directoryExists: (path2) => {
        const value = allowed(path2);
        return value !== void 0 && existsSync(value) && statSync(value).isDirectory();
      },
      realpath: (path2) => {
        const value = allowed(path2);
        return value === void 0 ? path2 : realpathSync(value);
      },
      readFile: (path2) => {
        const value = allowed(path2);
        if (value === void 0 || !existsSync(value)) return void 0;
        const raw = readFileSync(value, "utf8");
        const syntax = ts.parseJsonText(value, raw).parseDiagnostics;
        if (syntax.length > 0) throw diagnosticError(syntax);
        this.configReads.set(realpathSync(value), `sha256:${createHash("sha256").update(raw).digest("hex")}`);
        return raw;
      }
    };
    const parsed = ts.getParsedCommandLineOfConfigFile(config, {}, host);
    if (parsed === void 0) throw new Error(`Cannot read TypeScript config ${config}`);
    const errors = parsed.errors.filter((diagnostic) => diagnostic.code !== 18003);
    if (errors.length > 0) throw diagnosticError(errors);
    this.options.set(config, parsed.options);
    return parsed.options;
  }
};
function missing(error) {
  return error instanceof Error && "code" in error && (error.code === "ENOENT" || error.code === "ENOTDIR");
}
function diagnosticError(diagnostics) {
  return new Error(ts.formatDiagnostics(diagnostics, {
    getCanonicalFileName: (path) => path,
    getCurrentDirectory: () => "",
    getNewLine: () => "\n"
  }).trim());
}

// packages/module-execution/src/index.ts
var languageVersion = "helmr.module-execution.v0";
var typescriptVersion = "6.0.3";
var sourceExtensions = [".js", ".mjs", ".cjs", ".ts", ".tsx", ".mts", ".cts", ".jsx"];
var typed = (path) => /\.(?:[cm]?ts|tsx|jsx)$/.test(path);
var absent = (error) => error instanceof Error && "code" in error && (error.code === "ERR_MODULE_NOT_FOUND" || error.code === "MODULE_NOT_FOUND" || error.code === "ERR_UNSUPPORTED_DIR_IMPORT");
function installModuleExecution(options) {
  if (activeExecution !== void 0) throw new Error("Helmr module execution is already installed");
  if (ts2.version !== typescriptVersion) throw new Error(`Helmr requires TypeScript ${typescriptVersion}, got ${ts2.version}`);
  const authority = new SourceAuthority(options.root);
  authority.assertEntry();
  const configPath = join2(authority.root, "helmr.config.ts");
  const rootConfig = existsSync2(configPath) ? authority.file(configPath) : void 0;
  const platformFiles = new Set(options.platformRoot === void 0 ? [] : [
    resolve2(options.platformRoot, "helmr/entry.mjs"),
    resolve2(options.platformRoot, "moduleexecution/loader.mjs"),
    resolve2(options.platformRoot, "moduleexecution/typescript.cjs")
  ]);
  const source = (path) => {
    const canonical = authority.file(path);
    if (options.phase === "program" && canonical === rootConfig) {
      throw new Error(`helmr.config.ts is build-only and cannot be imported by Program modules: ${path}`);
    }
    return canonical;
  };
  function requestURL(specifier, context) {
    if (context.conditions.includes("require") && !specifier.startsWith("file:")) {
      const directory = context.parentURL === void 0 ? authority.root : dirname2(fileURLToPath(context.parentURL));
      return pathToFileURL(resolve2(directory, specifier));
    }
    return new URL(specifier, context.parentURL);
  }
  function candidate(specifier, url, context, next) {
    const path = authority.contained(fileURLToPath(url));
    try {
      return next(specifier, context);
    } catch (error) {
      if (!absent(error)) throw error;
      if (existsSync2(path)) {
        if (!statSync2(path).isDirectory() || existsSync2(authority.contained(join2(path, "package.json")))) throw error;
      }
      const extension = extname(path);
      const candidates = [];
      const replacement = { ".js": ".ts", ".mjs": ".mts", ".cjs": ".cts" }[extension];
      if (replacement !== void 0) candidates.push(path.slice(0, -extension.length) + replacement);
      if (extension === "") {
        candidates.push(...sourceExtensions.map((extension2) => path + extension2));
        candidates.push(...sourceExtensions.map((extension2) => join2(path, "index" + extension2)));
      }
      for (const value of candidates) {
        authority.contained(value);
        if (!existsSync2(value) || !statSync2(value).isFile()) continue;
        source(value);
        const target = pathToFileURL(value);
        target.search = url.search;
        target.hash = url.hash;
        return next(context.conditions.includes("require") ? value : target.href, context);
      }
      throw error;
    }
  }
  const formats = /* @__PURE__ */ new Map();
  const hooks = registerHooks({
    resolve(specifier, context, next) {
      if (isBuiltin(specifier)) return next(specifier, context);
      const parent = context.parentURL?.startsWith("file:") ? fileURLToPath(context.parentURL) : void 0;
      const customer = parent !== void 0 && isContained(authority.root, parent);
      const trusted = context.parentURL === void 0 || parent !== void 0 && platformFiles.has(parent);
      let result;
      if (customer && bareAlias(specifier)) {
        for (const path of aliases(specifier, authority.config(parent))) {
          authority.contained(path);
          try {
            const url = pathToFileURL(path);
            result = candidate(context.conditions.includes("require") ? path : url.href, url, context, next);
            break;
          } catch (error) {
            if (!absent(error) || existsSync2(path)) throw error;
          }
        }
      }
      if (result === void 0) {
        if (specifier.startsWith(".") || specifier.startsWith("/") || specifier.startsWith("file:")) {
          const url = requestURL(specifier, context);
          const target = fileURLToPath(url);
          if (!trusted || !platformFiles.has(target)) authority.contained(target);
          result = customer ? candidate(specifier, url, context, next) : next(specifier, context);
        } else result = next(specifier, context);
      }
      if (result.url.startsWith("file:")) {
        const path = fileURLToPath(result.url);
        if (!trusted || !platformFiles.has(path)) source(path);
      }
      return result;
    },
    load(url, context, next) {
      if (!url.startsWith("file:")) return next(url, context);
      const path = fileURLToPath(url);
      if (platformFiles.has(path)) return next(url, context);
      const canonical = source(path);
      if (!typed(canonical)) {
        const loaded = next(url, context);
        formats.set(url, loaded.format ?? void 0);
        return loaded;
      }
      if (/\.d\.(?:[cm]?ts)$/.test(canonical)) throw new Error(`Type declarations are not executable Program source: ${canonical}`);
      const format = authority.format(canonical);
      formats.set(url, format);
      const result = ts2.transpileModule(readFileSync2(canonical, "utf8"), {
        fileName: canonical,
        compilerOptions: emitOptions(authority.config(canonical), format),
        reportDiagnostics: true
      });
      const errors = result.diagnostics?.filter((diagnostic) => diagnostic.category === ts2.DiagnosticCategory.Error) ?? [];
      if (errors.length > 0) throw diagnosticError(errors);
      return { format, source: result.outputText, shortCircuit: true };
    }
  });
  const execution = {
    root: authority.root,
    rootConfig,
    configReads: authority.configReads,
    dispose: () => {
      hooks.deregister();
      if (activeExecution === execution) activeExecution = void 0;
    },
    async importSourceExports(url) {
      source(fileURLToPath(url));
      const namespace = await import(url.href);
      if (formats.get(url.href) !== "commonjs") return namespace;
      const value = namespace["default"];
      if (value !== null && (typeof value === "object" || typeof value === "function")) {
        const exports = value;
        return { ...exports, default: exports["__esModule"] === true ? exports["default"] : value };
      }
      return { default: value };
    }
  };
  activeExecution = execution;
  return execution;
}
function bareAlias(specifier) {
  return !specifier.startsWith(".") && !specifier.startsWith("/") && !specifier.startsWith("#") && !specifier.includes(":");
}
function aliases(specifier, options) {
  const paths = options.paths ?? {};
  let key = Object.hasOwn(paths, specifier) ? specifier : void 0;
  let wildcard = "";
  if (key === void 0) {
    const keys = Object.keys(paths).filter((value) => value.includes("*")).sort((a, b) => b.indexOf("*") - a.indexOf("*") || b.length - a.length);
    for (const value of keys) {
      const [prefix = "", suffix = ""] = value.split("*");
      if (!specifier.startsWith(prefix) || !specifier.endsWith(suffix) || specifier.length < prefix.length + suffix.length) continue;
      key = value;
      wildcard = specifier.slice(prefix.length, specifier.length - suffix.length);
      break;
    }
  }
  const base = options.baseUrl ?? options["pathsBasePath"];
  if (key !== void 0 && typeof base === "string") return paths[key].map((value) => resolve2(base, value.replace("*", wildcard)));
  return key === void 0 && options.baseUrl !== void 0 ? [resolve2(options.baseUrl, specifier)] : [];
}
function emitOptions(options, format) {
  const lowering = Object.fromEntries(Object.entries({
    jsxFactory: options.jsxFactory,
    jsxFragmentFactory: options.jsxFragmentFactory,
    jsxImportSource: options.jsxImportSource,
    useDefineForClassFields: options.useDefineForClassFields,
    experimentalDecorators: options.experimentalDecorators,
    emitDecoratorMetadata: options.emitDecoratorMetadata,
    alwaysStrict: options.alwaysStrict,
    importHelpers: options.importHelpers,
    noEmitHelpers: options.noEmitHelpers,
    removeComments: options.removeComments
  }).filter(([, value]) => value !== void 0));
  return {
    ...lowering,
    // Only lowering options apply; project-output/typechecking settings do not.
    target: Math.min(options.target ?? ts2.ScriptTarget.ES2022, ts2.ScriptTarget.ES2022),
    module: format === "module" ? ts2.ModuleKind.ESNext : ts2.ModuleKind.CommonJS,
    jsx: options.jsx === ts2.JsxEmit.Preserve || options.jsx === ts2.JsxEmit.ReactNative ? ts2.JsxEmit.React : options.jsx ?? ts2.JsxEmit.React,
    esModuleInterop: options.esModuleInterop ?? true,
    verbatimModuleSyntax: options.verbatimModuleSyntax ?? false,
    inlineSourceMap: true,
    inlineSources: true,
    sourceRoot: "",
    ignoreDeprecations: "6.0"
  };
}
var activeExecution;
function importSourceExports(url) {
  if (activeExecution === void 0) throw new Error("Helmr module execution has not been installed");
  return activeExecution.importSourceExports(url);
}
function moduleExecutionIdentity() {
  const digest = (url) => `sha256:${createHash2("sha256").update(readFileSync2(url)).digest("hex")}`;
  return { apiVersion: languageVersion, adapterDigest: digest(new URL(import.meta.url)), typescriptDigest: digest(new URL("./typescript.cjs", import.meta.url)), typescriptVersion };
}
export {
  importSourceExports,
  installModuleExecution,
  languageVersion,
  moduleExecutionIdentity,
  sourceExtensions,
  typescriptVersion
};
