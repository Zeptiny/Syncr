/// <reference types="vitest/config" />
import { fileURLToPath, URL } from "node:url";

import tailwindcss from "@tailwindcss/vite";
import { tanstackRouter } from "@tanstack/router-plugin/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// The controller serves the API under /syncr.v1.<Service>/ and /healthz.
// changeOrigin stays false so Origin matches Host for the controller's
// cross-origin protection; streaming responses pass through unbuffered.
const api = process.env.SYNCR_DEV_API ?? "http://127.0.0.1:8080";

export default defineConfig({
  plugins: [tanstackRouter({ target: "react", autoCodeSplitting: true }), react(), tailwindcss()],
  resolve: {
    alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) },
  },
  server: {
    proxy: {
      "^/syncr\\.v1\\.": { target: api, changeOrigin: false },
      "/healthz": { target: api, changeOrigin: false },
    },
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
    sourcemap: false,
    // index.html must not be inlined into: the CSP forbids inline scripts.
    assetsInlineLimit: 0,
    chunkSizeWarningLimit: 900,
  },
  test: {
    environment: "jsdom",
    setupFiles: ["./vitest.setup.ts"],
    include: ["src/**/*.test.{ts,tsx}"],
    css: false,
    restoreMocks: true,
    // Node 25+ defines its own globalThis.localStorage, which is unusable
    // without --localstorage-file and shadows jsdom's Storage. Turn it off
    // in the test workers so the tests use jsdom's.
    execArgv: ["--no-experimental-webstorage"],
  },
});
