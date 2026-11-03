import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";

// During `npm run dev` the panel runs next to Vite, on port 8090.
const panel = process.env.POCKETCLOUD_PANEL ?? "http://127.0.0.1:8090";

export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      "/api": { target: panel, changeOrigin: true, ws: true },
    },
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
    sourcemap: false,
  },
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: "./src/test-setup.ts",
    css: false,
  },
});
