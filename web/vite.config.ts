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
    chunkSizeWarningLimit: 300,
    rolldownOptions: {
      output: {
        // Routes are split by the router plugin (autoCodeSplitting). The
        // framework every page needs goes into a few vendor chunks, which
        // stay cached across deploys while the app chunk changes.
        codeSplitting: {
          groups: [
            { name: "react", test: /node_modules[\\/](react|react-dom|scheduler)[\\/]/ },
            { name: "rpc", test: /node_modules[\\/]@(bufbuild|connectrpc)[\\/]/ },
            {
              name: "tanstack",
              test: /node_modules[\\/]@tanstack[\\/](react-router|router-core|history|store|react-store|query-core|react-query)[\\/]/,
            },
          ],
        },
      },
    },
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
