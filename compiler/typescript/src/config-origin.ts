import { build, type Loader, type Plugin } from "esbuild"
import { readFile } from "node:fs/promises"
import { dirname, extname, relative } from "node:path"
import { pathToFileURL } from "node:url"

// Config executes in a temporary bundle, but source-relative resources belong
// to the original installed tree. Let esbuild bind injected names (including
// shadowing) and preserve its own per-input tsconfig semantics.
export function configOrigin(root: string, target: string): Plugin {
  return {
    name: "helmr-config-origin",
    setup(outer) {
      outer.onLoad({ filter: /\.[cm]?[jt]sx?$/ }, async ({ path }) => {
        const contents = await readFile(path, "utf8")
        const extension = extname(path)
        const loader: Loader = extension === ".tsx" ? "tsx"
          : extension === ".jsx" ? "jsx" : extension.endsWith("ts") ? "ts" : "js"
        const location = JSON.stringify(relative(root, path))
        const shim = `
          import { createRequire } from "node:module";
          const url = ${JSON.stringify(pathToFileURL(path).href)};
          const filename = ${JSON.stringify(path)};
          const dirname = ${JSON.stringify(dirname(path))};
          const meta = { url, filename, dirname, main: false, resolve() {
            throw new Error(${JSON.stringify(`Config source ${location}: import.meta.resolve() is unsupported; use a Node-ready installed JavaScript helper for native import resolution`)});
          }};
          const resolve = createRequire(url).resolve;
          export { meta as "import.meta", filename as "__filename",
            dirname as "__dirname", resolve as "require.resolve" };
        `
        const transformed = await build({
          absWorkingDir: root,
          entryPoints: [path],
          // Keep map sources relative to the original input's directory. This
          // output is virtual; the captured source is never overwritten.
          outfile: `${path}.helmr-origin.js`,
          bundle: false,
          write: false,
          platform: "node",
          target,
          sourcemap: "inline",
          sourcesContent: true,
          logLevel: "silent",
          inject: ["<helmr-config-origin>"],
          plugins: [{
            name: "helmr-captured-config-source",
            setup(inner) {
              inner.onResolve({ filter: /^<helmr-config-origin>$/ }, () => ({
                path, namespace: "helmr-origin",
              }))
              inner.onLoad({ filter: /.*/, namespace: "helmr-origin" }, () => ({
                contents: shim, loader: "js",
              }))
              inner.onLoad({ filter: /.*/ }, (args) => args.path === path
                ? { contents, loader, resolveDir: dirname(path) } : undefined)
            },
          }],
        })
        if (transformed.outputFiles?.length !== 1) {
          throw new Error("config origin transform must produce one virtual output")
        }
        return {
          contents: transformed.outputFiles[0]!.text,
          loader: "js",
          resolveDir: dirname(path),
        }
      })
    },
  }
}
