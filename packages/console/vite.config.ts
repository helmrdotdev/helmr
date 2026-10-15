import { defineConfig, type Plugin } from "vite";
import tailwindcss from "@tailwindcss/vite";
import solid from "vite-plugin-solid";

const backendURL = process.env["HELMR_DEV_BACKEND_URL"] ?? "http://127.0.0.1:8080";
const devPort = Number.parseInt(process.env["HELMR_DEV_CONSOLE_PORT"] ?? "3000", 10);

// Dev server only, for reviewing loading and failure states in this browser:
// `/__dev/delay?ms=1500` slows every API response (`ms=0` off), and
// `/__dev/fail?match=sessions&status=500` answers API requests whose path
// contains `match` with that error status (`/__dev/fail` alone turns it off).
function devStates(): Plugin {
  const cookie = (header: string | undefined, name: string) =>
    new RegExp(`(?:^|;\\s*)${name}=([^;]*)`).exec(header ?? "")?.[1];
  const redirect = (res: import("node:http").ServerResponse, next: string | null, cookies: string[]) => {
    const target = next ?? "/";
    res.statusCode = 302;
    res.setHeader("Set-Cookie", cookies);
    res.setHeader("Location", /^\/(?![/\\])/.test(target) ? target : "/");
    res.end();
  };
  const set = (name: string, value: string) => `${name}=${encodeURIComponent(value)}; Path=/; SameSite=Lax${value === "" ? "; Max-Age=0" : ""}`;
  return {
    name: "helmr-dev-states",
    apply: "serve",
    configureServer(server) {
      server.middlewares.use((req, res, next) => {
        const url = new URL(req.url ?? "/", "http://dev");
        if (url.pathname === "/__dev/delay") {
          const ms = Math.min(60_000, Math.max(0, Number.parseInt(url.searchParams.get("ms") ?? "0", 10) || 0));
          redirect(res, url.searchParams.get("next"), [set("helmr_dev_delay", ms === 0 ? "" : String(ms))]);
          return;
        }
        if (url.pathname === "/__dev/fail") {
          const match = url.searchParams.get("match") ?? "";
          const status = url.searchParams.get("status") ?? "500";
          redirect(res, url.searchParams.get("next"), [set("helmr_dev_fail", match === "" ? "" : `${status}:${match}`)]);
          return;
        }
        if (!url.pathname.startsWith("/api/")) {
          next();
          return;
        }
        const fail = decodeURIComponent(cookie(req.headers.cookie, "helmr_dev_fail") ?? "");
        const separator = fail.indexOf(":");
        if (separator > 0 && url.pathname.includes(fail.slice(separator + 1))) {
          const status = Number.parseInt(fail.slice(0, separator), 10) || 500;
          const code = status === 403 ? "forbidden" : status === 404 ? "not_found" : "internal_error";
          res.statusCode = status;
          res.setHeader("Content-Type", "application/json");
          res.end(JSON.stringify({ error: { code, message: `simulated ${status} for ${url.pathname}` } }));
          return;
        }
        const ms = Number.parseInt(cookie(req.headers.cookie, "helmr_dev_delay") ?? "0", 10) || 0;
        if (ms > 0) {
          setTimeout(next, Math.min(ms, 60_000));
          return;
        }
        next();
      });
    },
  };
}

export default defineConfig({
  plugins: [tailwindcss(), solid(), devStates()],
  server: {
    host: "127.0.0.1",
    port: devPort,
    strictPort: true,
    proxy: {
      "/api/": {
        target: backendURL,
        changeOrigin: true,
      },
      "/dev/": {
        target: backendURL,
        changeOrigin: true,
      },
    },
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
  },
});
