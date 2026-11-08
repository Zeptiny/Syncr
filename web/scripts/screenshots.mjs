#!/usr/bin/env node
// Takes the PR screenshots of the web UI with playwright-core and the
// Chromium build already cached on the host (no download).
//
// Environment:
//   SYNCR_UI_URL            UI base URL (default http://127.0.0.1:5173)
//   SYNCR_SETUP_URL         a fresh controller still in first-run setup, for
//                           01-setup (default: skipped unless SYNCR_UI_MOCK=1)
//   SYNCR_UI_USER           sign-in user (default admin)
//   SYNCR_UI_PASSWORD       sign-in password (default syncr-demo-admin)
//   SYNCR_SCREENSHOT_DIR    output directory (default ../docs/screenshots/m3)
//   SYNCR_SCREENSHOT_ONLY   comma-separated shot names to take (default all)
//   SYNCR_SCREENSHOT_MOBILE 1 to also take 390px-wide shots
//   SYNCR_SCREENSHOT_FULL   1 to capture whole pages instead of the viewport
//   SYNCR_UI_MOCK           1 when the UI runs with VITE_SYNCR_MOCK=1
//   CHROMIUM_PATH           Chromium executable (default: the cached
//                           ~/.cache/ms-playwright/chromium-1140 build)
/* global window -- used inside page.addInitScript, which runs in the browser */
import { mkdir } from "node:fs/promises";
import { homedir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { chromium } from "playwright-core";

const here = dirname(fileURLToPath(import.meta.url));
const base = (process.env.SYNCR_UI_URL ?? "http://127.0.0.1:5173").replace(/\/$/, "");
const mock = process.env.SYNCR_UI_MOCK === "1";
const setupUrl = (process.env.SYNCR_SETUP_URL ?? (mock ? base : "")).replace(/\/$/, "");
const user = process.env.SYNCR_UI_USER ?? "admin";
const password = process.env.SYNCR_UI_PASSWORD ?? "syncr-demo-admin";
const outDir = resolve(process.env.SYNCR_SCREENSHOT_DIR ?? join(here, "../../docs/screenshots/m3"));
const only = process.env.SYNCR_SCREENSHOT_ONLY?.split(",").filter(Boolean);
const mobile = process.env.SYNCR_SCREENSHOT_MOBILE === "1";
const fullPage = process.env.SYNCR_SCREENSHOT_FULL === "1";
const executablePath =
  process.env.CHROMIUM_PATH ??
  join(homedir(), ".cache/ms-playwright/chromium-1140/chrome-linux/chrome");

const wanted = (name) => !only || only.some((o) => name.includes(o));

async function newPage(browser, { width, height, dark = false, init } = {}) {
  const context = await browser.newContext({
    viewport: { width: width ?? 1440, height: height ?? 900 },
    deviceScaleFactor: 1,
    colorScheme: dark ? "dark" : "light",
    reducedMotion: "reduce",
  });
  await context.addInitScript(
    ({ theme, init }) => {
      try {
        window.localStorage.setItem("syncr-theme", theme);
        for (const [k, v] of Object.entries(init ?? {})) window.localStorage.setItem(k, v);
      } catch {
        // ignore
      }
    },
    { theme: dark ? "dark" : "light", init },
  );
  return context.newPage();
}

async function settle(page, ms = 600) {
  await page.waitForLoadState("networkidle").catch(() => {});
  await page.waitForTimeout(ms);
}

async function shot(page, name) {
  const file = join(outDir, `${name}.png`);
  await page.screenshot({ path: file, fullPage });
  console.log(`saved ${file}`);
}

async function signIn(page) {
  await page.goto(`${base}/login`);
  await page.locator("#username").fill(user);
  await page.locator("#password").fill(password);
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(/\/jobs/);
  await settle(page);
}

// openRow opens a list row: by its accessible name (exact), or else the
// first row containing the text.
async function openRow(page, path, text, { exact = false } = {}) {
  await page.goto(`${base}${path}`);
  await settle(page);
  const row = exact
    ? page.getByRole("row", { name: text, exact: true })
    : page.locator("tbody tr", { hasText: text });
  await row.first().click();
  await settle(page);
}

const shots = [
  ["03-jobs", async (p) => (await p.goto(`${base}/jobs`), settle(p))],
  [
    "04-job-live",
    async (p) => {
      await openRow(p, "/jobs", "Running");
      await p.waitForTimeout(4000);
    },
  ],
  ["05-run-job", async (p) => (await p.goto(`${base}/jobs/new`), settle(p))],
  ["06-remotes", async (p) => (await p.goto(`${base}/remotes`), settle(p))],
  [
    "07-remote-form-s3",
    async (p) => {
      await p.goto(`${base}/remotes/new?type=s3`);
      await settle(p);
      await p.getByLabel("Name").first().fill("Wasabi archive");
      await p.getByLabel("Provider").click();
      await p.getByRole("option", { name: "Wasabi" }).click();
      await p.getByLabel("Access key ID").fill("WSB4K2Q9EXAMPLE");
      await p.getByLabel("Region").fill("eu-central-1");
      await p.getByLabel("Endpoint").fill("https://s3.eu-central-1.wasabisys.com");
      await p.getByRole("button", { name: /Advanced options/ }).click();
      await settle(p, 300);
    },
  ],
  ["08-remote-union", async (p) => openRow(p, "/remotes", "Pooled disks", { exact: true })],
  ["09-workers", async (p) => (await p.goto(`${base}/workers`), settle(p))],
  [
    "10-add-worker-active",
    async (p) => {
      await p.goto(`${base}/workers/new`);
      await settle(p);
      await p.getByRole("button", { name: /Generate install command/ }).click();
      await p.getByRole("button", { name: "Copy command" }).first().waitFor();
      await settle(p, 300);
    },
  ],
  [
    "11-add-worker-passive",
    async (p) => (await p.goto(`${base}/workers/new?tab=passive`), settle(p)),
  ],
  ["12-users", async (p) => (await p.goto(`${base}/users`), settle(p))],
  [
    "13-settings-tokens",
    async (p) => {
      await p.goto(`${base}/settings`);
      await settle(p);
      // The dev seed has no API tokens: create one so the list is not empty.
      if (await p.getByText("No API tokens").isVisible()) {
        await p.getByRole("button", { name: "Create token" }).click();
        await p.locator("#tok-name").fill("Home Assistant");
        await p.getByRole("dialog").getByRole("button", { name: "Create token" }).click();
        await p.getByRole("button", { name: "I've copied it" }).click();
        await settle(p);
      }
    },
  ],
];

async function main() {
  await mkdir(outDir, { recursive: true });
  const browser = await chromium.launch({ executablePath, args: ["--no-sandbox"] });
  try {
    if (setupUrl && wanted("01-setup")) {
      const p = await newPage(browser, { init: mock ? { "syncr.mock.setup": "1" } : undefined });
      await p.goto(`${setupUrl}/setup`);
      await settle(p);
      await shot(p, "01-setup");
      await p.context().close();
    }
    if (wanted("02-login")) {
      const p = await newPage(browser);
      await p.goto(`${base}/login`);
      await settle(p);
      await shot(p, "02-login");
      await p.context().close();
    }

    const variants = [{ suffix: "", width: 1440, height: 900 }];
    if (mobile) variants.push({ suffix: "-mobile", width: 390, height: 844 });
    for (const v of variants) {
      const p = await newPage(browser, v);
      await signIn(p);
      for (const [name, go] of shots) {
        if (!wanted(name)) continue;
        await go(p);
        await shot(p, name + v.suffix);
      }
      await p.context().close();
    }

    if (wanted("04-job-live")) {
      const p = await newPage(browser, { dark: true });
      await signIn(p);
      await openRow(p, "/jobs", "Running");
      await p.waitForTimeout(4000);
      await shot(p, "04-job-live-dark");
      await p.context().close();
    }
  } finally {
    await browser.close();
  }
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
