import "./index.css";

import type { Transport } from "@connectrpc/connect";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

import { createTransport } from "@/lib/transport";
import { App, createApp } from "@/router";

async function transport(): Promise<Transport> {
  // Dev-only: VITE_SYNCR_MOCK=1 serves fixtures instead of the controller.
  // import.meta.env.DEV is false in production builds, so the mock and its
  // fixtures are removed from the bundle.
  if (import.meta.env.DEV && import.meta.env.VITE_SYNCR_MOCK === "1") {
    const { createMockTransport } = await import("@/mock/transport");
    return createMockTransport();
  }
  return createTransport();
}

void transport().then((t) => {
  const app = createApp(t);
  const root = document.getElementById("root");
  if (!root) throw new Error("missing #root");
  createRoot(root).render(
    <StrictMode>
      <App {...app} />
    </StrictMode>,
  );
});
