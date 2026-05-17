import { defineConfig } from "vite";
import tailwindcss from "@tailwindcss/vite";
import solid from "vite-plugin-solid";

const backendURL = process.env["HELMR_DEV_BACKEND_URL"] ?? "http://127.0.0.1:8080";
const devPort = Number.parseInt(process.env["HELMR_DEV_CONSOLE_PORT"] ?? "3000", 10);

export default defineConfig({
  plugins: [tailwindcss(), solid()],
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
