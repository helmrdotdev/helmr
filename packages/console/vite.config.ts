import { defineConfig, type Plugin } from "vite";
import tailwindcss from "@tailwindcss/vite";
import solid from "vite-plugin-solid";

const backendURL = process.env["HELMR_DEV_BACKEND_URL"] ?? "http://127.0.0.1:8080";
const devPort = Number.parseInt(process.env["HELMR_DEV_CONSOLE_PORT"] ?? "3000", 10);

// Dev server only: slow every API response to review loading states.
// `/__dev/delay?ms=1500` turns it on for this browser, `/__dev/delay?ms=0` off.
function apiDelay(): Plugin {
  return {
    name: "helmr-dev-api-delay",
    apply: "serve",
    configureServer(server) {
      server.middlewares.use((req, res, next) => {
        const url = new URL(req.url ?? "/", "http://dev");
        if (url.pathname === "/__dev/delay") {
          const ms = Math.max(0, Number.parseInt(url.searchParams.get("ms") ?? "0", 10) || 0);
          res.statusCode = 302;
          res.setHeader("Set-Cookie", `helmr_dev_delay=${ms}; Path=/; SameSite=Lax${ms === 0 ? "; Max-Age=0" : ""}`);
          res.setHeader("Location", url.searchParams.get("next") ?? "/");
          res.end();
          return;
        }
        const ms = Number.parseInt(/(?:^|;\s*)helmr_dev_delay=(\d+)/.exec(req.headers.cookie ?? "")?.[1] ?? "0", 10);
        if (ms > 0 && url.pathname.startsWith("/api/")) {
          setTimeout(next, ms);
          return;
        }
        next();
      });
    },
  };
}

export default defineConfig({
  plugins: [tailwindcss(), solid(), apiDelay()],
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
