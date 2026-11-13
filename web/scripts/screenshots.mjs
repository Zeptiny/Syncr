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
//   SYNCR_SCREENSHOT_SET    m6 (default), m5, m4, m3, hooks, bisync or readme:
//                           which shot list
//   SYNCR_SCREENSHOT_DIR    output directory (default ../docs/screenshots/<set>)
//   SYNCR_SCREENSHOT_ONLY   comma-separated shot names to take (default all)
//   SYNCR_SCREENSHOT_MOBILE 1 to also take 390px-wide shots
//   SYNCR_SCREENSHOT_FULL   1 to capture whole pages instead of the viewport
//   SYNCR_UI_MOCK           1 when the UI runs with VITE_SYNCR_MOCK=1
//   CHROMIUM_PATH           Chromium executable (default: the first cached
//                           ~/.cache/ms-playwright build found, 1217 or 1140;
//                           set it when the host caches another build)
/* global window -- used inside page.addInitScript, which runs in the browser */
import { existsSync } from "node:fs";
import { mkdir } from "node:fs/promises";
import { homedir } from "node:os";
import { dirname, join, relative, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { chromium } from "playwright-core";

const here = dirname(fileURLToPath(import.meta.url));
const base = (process.env.SYNCR_UI_URL ?? "http://127.0.0.1:5173").replace(/\/$/, "");
const mock = process.env.SYNCR_UI_MOCK === "1";
const setupUrl = (process.env.SYNCR_SETUP_URL ?? (mock ? base : "")).replace(/\/$/, "");
const user = process.env.SYNCR_UI_USER ?? "admin";
const password = process.env.SYNCR_UI_PASSWORD ?? "syncr-demo-admin";
const set = ["m3", "m4", "m5", "hooks", "bisync", "readme"].includes(
  process.env.SYNCR_SCREENSHOT_SET,
)
  ? process.env.SYNCR_SCREENSHOT_SET
  : "m6";
const outDir = resolve(
  process.env.SYNCR_SCREENSHOT_DIR ?? join(here, `../../docs/screenshots/${set}`),
);
const only = process.env.SYNCR_SCREENSHOT_ONLY?.split(",").filter(Boolean);
const mobile = process.env.SYNCR_SCREENSHOT_MOBILE === "1";
const fullPage = process.env.SYNCR_SCREENSHOT_FULL === "1";
const cachedChromium = [
  ".cache/ms-playwright/chromium-1217/chrome-linux64/chrome",
  ".cache/ms-playwright/chromium-1140/chrome-linux/chrome",
].map((p) => join(homedir(), p));
const executablePath =
  process.env.CHROMIUM_PATH ?? cachedChromium.find((p) => existsSync(p)) ?? cachedChromium[0];

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
  // Relative, so the log carries no home directory.
  console.log(`saved ${relative(process.cwd(), file)}`);
}

async function signIn(page) {
  await page.goto(`${base}/login`);
  await page.locator("#username").fill(user);
  await page.locator("#password").fill(password);
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL((u) => !u.pathname.startsWith("/login"));
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

const m3Shots = [
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

// scrollTo brings a page section to the top of the viewport.
async function scrollTo(page, locator, offset = 16) {
  await locator.first().evaluate((el, off) => {
    window.scrollTo({ top: el.getBoundingClientRect().top + window.scrollY - off });
  }, offset);
  await page.waitForTimeout(250);
}

// pickTimezone opens the time zone picker and chooses a zone by search.
async function pickTimezone(page, search, zone) {
  await page.locator("#sched-tz").click();
  await page.getByLabel("Search time zones").fill(search);
  await page
    .getByRole("option", { name: new RegExp(zone) })
    .first()
    .click();
}

// The M4 shots. They expect the dev seed of the integration pass (see the
// track D report): schedules "Nightly photos → B2", "Office docs hourly",
// "Weekly archive", "Offsite mirror" and a paused one; contacts "Ops
// Discord", "Admin mail" and "Home Assistant"; worker nas-01 with labels.
const m4Shots = [
  ["01-dashboard", async (p) => (await p.goto(`${base}/?range=7d`), settle(p, 900))],
  ["03-schedules", async (p) => (await p.goto(`${base}/schedules`), settle(p))],
  [
    "04-schedule-new-cron",
    async (p) => {
      await p.goto(`${base}/schedules/new`);
      await settle(p);
      await p.locator("#sched-name").fill("Archive every other month");
      await pickTimezone(p, "berlin", "Berlin");
      await p.locator("#sched-cron").fill("30 2 1 */2 *");
      await settle(p, 900);
    },
  ],
  [
    "05-schedule-new-policies",
    async (p) => {
      await p.goto(`${base}/schedules/new`);
      await settle(p);
      await p.getByRole("radio", { name: "Any worker with a label" }).click();
      await p.locator("#sched-target-label").fill("nas");
      await p.getByRole("radio", { name: "Wait, then run" }).click();
      await scrollTo(p, p.getByRole("heading", { name: "Notifications" }), 300);
      await p.getByRole("combobox", { name: "Notify a contact" }).click();
      await p.getByRole("option", { name: "Ops Discord" }).click();
      await settle(p, 300);
      await scrollTo(p, p.getByRole("heading", { name: "Where it runs" }));
    },
  ],
  [
    "06-timezone-picker",
    async (p) => {
      await p.goto(`${base}/schedules/new`);
      await settle(p);
      await scrollTo(p, p.getByRole("heading", { name: "When" }), 96);
      await p.locator("#sched-tz").click();
      await p.getByLabel("Search time zones").fill("america");
      await settle(p, 300);
    },
  ],
  ["07-schedule-detail", async (p) => openRow(p, "/schedules", "Nightly photos")],
  ["08-notifications", async (p) => (await p.goto(`${base}/notifications`), settle(p))],
  [
    "09-contact-discord",
    async (p) => {
      await p.goto(`${base}/notifications/new?type=discord`);
      await settle(p);
      await p.getByLabel("Name").first().fill("Team Discord");
      await p
        .getByLabel("Webhook URL", { exact: true })
        .fill("https://discord.com/api/webhooks/1290000000000000000/aBcDeFgHiJkLmNoPqRsTuVwXyZ");
      await p.getByRole("button", { name: /Advanced options/ }).click();
      await p.getByRole("checkbox", { name: "Failed" }).click();
      await settle(p, 300);
    },
  ],
  [
    "10-contact-email-test",
    async (p) => {
      await openRow(p, "/notifications", "Admin mail");
      await p.getByRole("button", { name: "Send test" }).click();
      await p.getByText(/Test notification sent|was not delivered/).waitFor({ timeout: 20_000 });
      await settle(p, 300);
      await scrollTo(p, p.getByRole("button", { name: "Send test" }), 360);
    },
  ],
  [
    "11-contact-deliveries",
    async (p) => (await p.goto(`${base}/notifications?tab=deliveries`), settle(p)),
  ],
  [
    "12-worker-labels",
    async (p) => {
      await openRow(p, "/workers", "nas-01");
      await p.getByRole("button", { name: "Edit labels" }).click();
      await p.locator("#worker-labels").fill("media");
      await settle(p, 300);
      await scrollTo(p, p.getByRole("heading", { name: "Labels" }), 260);
    },
  ],
  [
    "13-jobs-scheduled",
    async (p) => {
      await p.goto(`${base}/jobs`);
      await settle(p);
      await p.getByRole("combobox", { name: "Schedule" }).click();
      await p.getByRole("option", { name: "Offsite mirror" }).click();
      await settle(p);
    },
  ],
];

// openSnapshot opens the newest snapshot of a repository in the browser and
// walks into the given folders.
async function openSnapshot(page, repository, folders) {
  await openRow(page, "/repositories", repository, { exact: true });
  await page.getByRole("table", { name: "Snapshots" }).locator("tbody tr").first().click();
  await settle(page);
  for (const f of folders) {
    await page.getByRole("button", { name: f, exact: true }).click();
    await settle(page, 400);
  }
}

// The M5 shots. They expect the dev seed of the integration pass (see the
// track D report): repositories "Photos vault" (on "B2 offsite"), "NAS
// documents" (on "MinIO archive", with a running and a waiting backup),
// "Office laptop" and "Old laptop" (deleting); workers nas-01 (roots /srv
// and /mnt) and office-pc (windows); the fake probes of devseed.Probes.
const m5Shots = [
  ["01-repositories", async (p) => (await p.goto(`${base}/repositories`), settle(p))],
  [
    "02-repository-new",
    async (p) => {
      await p.goto(`${base}/repositories/new`);
      await settle(p);
      await p.getByRole("radio", { name: /^Connect to an existing one/ }).click();
      await p.getByLabel("Name").first().fill("Legacy archive");
      await p.getByRole("combobox", { name: "Remote" }).click();
      await p.getByRole("option", { name: /^MinIO archive/ }).click();
      await p.getByRole("button", { name: /^Browse/ }).click();
      await p.getByRole("dialog").getByRole("button", { name: "restic", exact: true }).click();
      await settle(p, 500);
    },
  ],
  [
    "03-repository-detail",
    async (p) => openRow(p, "/repositories", "Photos vault", { exact: true }),
  ],
  [
    "04-snapshot-browser",
    async (p) => {
      await openSnapshot(p, "Photos vault", ["srv", "photos", "2026"]);
      for (const n of ["01", "02", "03"])
        await p.getByRole("checkbox", { name: `Select ${n}` }).click();
      await settle(p, 300);
    },
  ],
  [
    "05-restore-sheet",
    async (p) => {
      await openSnapshot(p, "Photos vault", ["srv", "photos", "2026"]);
      for (const n of ["01", "02", "03"])
        await p.getByRole("checkbox", { name: `Select ${n}` }).click();
      await p.getByRole("button", { name: /^Restore selected/ }).click();
      await p.getByRole("radiogroup", { name: "If a file already exists" }).waitFor();
      await settle(p, 400);
    },
  ],
  [
    "06-backup-form",
    async (p) => {
      await p.goto(`${base}/jobs/new?kind=restic_backup`);
      await settle(p);
      await p.getByRole("combobox", { name: /^Worker/ }).click();
      await p.getByRole("option", { name: /office-pc/ }).click();
      await p.getByRole("combobox", { name: /^Repository/ }).click();
      await p.getByRole("option", { name: /^Office laptop/ }).click();
      await p.getByLabel("Path to back up 1", { exact: true }).fill("C:\\Users\\ada\\Documents");
      await p.getByRole("button", { name: "Add a path" }).click();
      await p.getByLabel("Path to back up 2", { exact: true }).fill("D:\\Shared\\Projects");
      await p.getByRole("switch", { name: "Use a volume snapshot (VSS)" }).click();
      await settle(p, 300);
      await scrollTo(p, p.getByRole("heading", { name: "Where it runs" }));
    },
  ],
  [
    "07-path-browser-worker",
    async (p) => {
      await openRow(p, "/workers", "nas-01");
      await p.getByRole("button", { name: "Browse files" }).click();
      await p.getByRole("dialog").getByRole("button", { name: "/srv", exact: true }).click();
      await settle(p, 500);
    },
  ],
  [
    "08-schedule-retention",
    async (p) => {
      await p.goto(`${base}/schedules/new?kind=restic_forget`);
      await settle(p);
      await p.getByLabel("Name").first().fill("Weekly retention: documents");
      await p.getByRole("combobox", { name: /^Repository/ }).click();
      await p.getByRole("option", { name: /^NAS documents/ }).click();
      await settle(p, 300);
      await scrollTo(p, p.getByRole("heading", { name: "What kind of job" }));
    },
  ],
  [
    "09-job-backup-detail",
    async (p) => {
      // Succeeded backups only (status 4), newest first.
      await openRow(p, "/jobs?kind=restic_backup&status=4", "Back up /srv/photos");
      await scrollTo(p, p.getByRole("heading", { name: "Backup summary" }), 220);
    },
  ],
  [
    "10-jobs-waiting",
    async (p) => {
      await p.goto(`${base}/jobs`);
      await settle(p);
      await p.getByRole("combobox", { name: "Repository" }).click();
      await p.getByRole("option", { name: "NAS documents" }).click();
      await settle(p);
    },
  ],
  [
    "11-remote-test",
    async (p) => {
      await openRow(p, "/remotes", "B2 offsite", { exact: true });
      await p.getByRole("button", { name: "Test remote" }).click();
      await p.getByText(/The remote answered|The test failed/).waitFor({ timeout: 20_000 });
      await settle(p, 300);
      await scrollTo(p, p.getByRole("button", { name: "Test remote" }), 420);
    },
  ],
  ["12-jobs-restic", async (p) => (await p.goto(`${base}/jobs`), settle(p))],
  [
    "15-repository-delete-dialog",
    async (p) => {
      await openRow(p, "/repositories", "Photos vault", { exact: true });
      await p.getByRole("button", { name: "Delete", exact: true }).click();
      await p.getByRole("checkbox", { name: /Also delete the backup data/ }).click();
      await p.getByLabel(/to confirm/).fill("Photos vault");
      await settle(p, 300);
    },
  ],
  [
    "16-repository-deleting",
    async (p) => {
      await openRow(p, "/repositories", "Old laptop", { exact: true });
      await p.getByText(/Deleting the backup data/).waitFor();
      await settle(p, 500);
    },
  ],
];

// m5 takes the milestone 5 shots: light at 1440×900, then the repository
// page in dark and the snapshot browser at phone width (and every shot at
// 390 px with MOBILE=1).
async function m5(browser) {
  const variants = [{ suffix: "", width: 1440, height: 900 }];
  if (mobile) variants.push({ suffix: "-mobile", width: 390, height: 844 });
  for (const v of variants) {
    const p = await newPage(browser, v);
    await signIn(p);
    for (const [name, go] of m5Shots) {
      if (!wanted(name)) continue;
      await go(p);
      await shot(p, name + v.suffix);
    }
    await p.context().close();
  }
  if (wanted("13-repository-detail-dark")) {
    const p = await newPage(browser, { dark: true });
    await signIn(p);
    await openRow(p, "/repositories", "Photos vault", { exact: true });
    await shot(p, "13-repository-detail-dark");
    await p.context().close();
  }
  if (wanted("14-snapshot-browser-mobile") && !mobile) {
    const p = await newPage(browser, { width: 390, height: 844 });
    await signIn(p);
    await openSnapshot(p, "Photos vault", ["srv", "photos"]);
    // The snapshot's details come first at this width: show the files.
    await scrollTo(p, p.getByRole("button", { name: /^Restore this folder/ }), 72);
    await shot(p, "14-snapshot-browser-mobile");
    await p.context().close();
  }
}

// sftpForm opens a new SFTP remote with its server filled in, so "Fetch
// from server" can run.
async function sftpForm(page) {
  await page.goto(`${base}/remotes/new?type=sftp`);
  await settle(page);
  await page.getByLabel("Name").first().fill("NAS over SSH");
  await page.getByLabel("Host", { exact: true }).fill("nas.example.net");
  await page.getByLabel("User", { exact: true }).fill("backup");
}

// fetchKeys presses "Fetch from server" and waits for the fingerprints.
async function fetchKeys(page) {
  await page.getByRole("button", { name: "Fetch from server" }).click();
  await page.getByText(/The server offered|Could not read the server/).waitFor({ timeout: 20_000 });
  await settle(page, 300);
}

// The M6 shots. They expect the dev seed of the integration pass (see the
// step D report): a transfer that failed by its 6h time limit (source path
// "raw"); the SFTP remotes "Storage box" (no host keys) and "Offsite SSH"
// (with keys); the online, outdated worker attic-nas (linux/amd64); a
// canned sftp_host_keys probe answer.
const m6Shots = [
  [
    "01-time-limit-field",
    async (p) => {
      await p.goto(`${base}/jobs/new`);
      await settle(p);
      await p.getByRole("textbox", { name: /^Stop the job after/ }).fill("6");
      await settle(p, 300);
      await scrollTo(p, p.getByRole("heading", { name: "Options" }));
    },
  ],
  [
    "02-job-failed-time-limit",
    // Failed jobs only (status 5) whose paths mention raw.
    async (p) => openRow(p, "/jobs?status=5&q=raw", "Photos"),
  ],
  [
    "03-sftp-form-unverified",
    async (p) => {
      await sftpForm(p);
      await scrollTo(p, p.locator(`[data-field$="-host_keys"]`), 220);
    },
  ],
  [
    "04-sftp-form-keys-fetched",
    async (p) => {
      await sftpForm(p);
      await fetchKeys(p);
      await scrollTo(p, p.locator(`[data-field$="-host_keys"]`), 96);
    },
  ],
  ["05-remotes-unverified", async (p) => (await p.goto(`${base}/remotes`), settle(p))],
  ["06-workers-outdated", async (p) => (await p.goto(`${base}/workers`), settle(p))],
  [
    "07-worker-upgrade",
    async (p) => {
      await openRow(p, "/workers", "attic-nas");
      await p.getByRole("button", { name: "Copy command" }).first().waitFor();
      await settle(p, 300);
    },
  ],
];

// m6 takes the milestone 6 shots: light at 1440×900, then the upgrade card
// in dark and the fetched host keys at phone width (and every shot at
// 390 px with MOBILE=1).
async function m6(browser) {
  const variants = [{ suffix: "", width: 1440, height: 900 }];
  if (mobile) variants.push({ suffix: "-mobile", width: 390, height: 844 });
  for (const v of variants) {
    const p = await newPage(browser, v);
    await signIn(p);
    for (const [name, go] of m6Shots) {
      if (!wanted(name)) continue;
      await go(p);
      await shot(p, name + v.suffix);
    }
    await p.context().close();
  }
  if (wanted("08-worker-upgrade-dark")) {
    const p = await newPage(browser, { dark: true });
    await signIn(p);
    await openRow(p, "/workers", "attic-nas");
    await p.getByRole("button", { name: "Copy command" }).first().waitFor();
    await settle(p, 300);
    await shot(p, "08-worker-upgrade-dark");
    await p.context().close();
  }
  if (wanted("09-sftp-keys-mobile") && !mobile) {
    const p = await newPage(browser, { width: 390, height: 844 });
    await signIn(p);
    await sftpForm(p);
    await fetchKeys(p);
    // Below the 56 px header: the panel's title, then its keys and button.
    await scrollTo(p, p.getByText(/The server offered/), 72);
    await shot(p, "09-sftp-keys-mobile");
    await p.context().close();
  }
}

// pickHook adds a hook to one of the four lists of a job form.
async function pickHook(page, list, hook) {
  await page.getByRole("combobox", { name: `Add a hook: ${list}` }).click();
  await page.getByRole("option", { name: new RegExp(`^${hook}`) }).click();
}

// hooksBackupForm fills the backup form for the documents on nas-01, up to
// its hooks.
async function hooksBackupForm(page) {
  await page.goto(`${base}/jobs/new?kind=restic_backup`);
  await settle(page);
  await page.getByRole("combobox", { name: /^Worker/ }).click();
  await page.getByRole("option", { name: /nas-01/ }).click();
  await page.getByRole("combobox", { name: /^Repository/ }).click();
  await page.getByRole("option", { name: /^NAS documents/ }).click();
  await page.getByLabel("Path to back up 1", { exact: true }).fill("/srv/docs");
  await pickHook(page, "before the job", "stop-paperless");
  await pickHook(page, "before the job", "dump-paperless-db");
  await pickHook(page, "after success", "ping-healthcheck");
  await pickHook(page, "always, at the end", "start-paperless");
  await settle(page, 300);
}

// openHookedBackup opens a finished backup of the NAS documents with the
// given status (4 succeeded, 5 failed) and brings its hooks into view. The
// status is quoted as the app writes it: the router reads a bare number as
// a number, and the list drops a status that is not a string.
async function openHookedBackup(page, status) {
  await openRow(page, `/jobs?kind=restic_backup&status=%22${status}%22`, "Back up /srv/docs");
  await page.getByRole("heading", { name: "Hooks" }).waitFor();
  await scrollTo(page, page.getByRole("heading", { name: "Hooks" }), 96);
}

// The hooks shots. They expect the dev seed: nas-01 defines four hooks,
// the backups of the NAS documents ran them, and one of those failed
// because its last hook timed out.
const hooksShots = [
  [
    "01-job-form-hooks",
    async (p) => {
      await hooksBackupForm(p);
      await scrollTo(p, p.getByRole("heading", { name: "Hooks" }), 96);
    },
  ],
  [
    "02-job-form-hook-choices",
    async (p) => {
      await hooksBackupForm(p);
      await scrollTo(p, p.getByRole("heading", { name: "Hooks" }), 96);
      await p.getByRole("combobox", { name: "Add a hook: after failure" }).click();
      await settle(p, 300);
    },
  ],
  ["03-job-hook-failed", async (p) => openHookedBackup(p, 5)],
  ["04-job-hooks-succeeded", async (p) => openHookedBackup(p, 4)],
  [
    "05-worker-hooks",
    async (p) => {
      await openRow(p, "/workers", "nas-01");
      await scrollTo(p, p.getByRole("heading", { name: "Hooks" }), 96);
    },
  ],
];

// hooks takes the hooks shots: light at 1440×900, then the failed job in
// dark and the form's hooks at phone width (and every shot at 390 px with
// MOBILE=1).
async function hooks(browser) {
  const variants = [{ suffix: "", width: 1440, height: 900 }];
  if (mobile) variants.push({ suffix: "-mobile", width: 390, height: 844 });
  for (const v of variants) {
    const p = await newPage(browser, v);
    await signIn(p);
    for (const [name, go] of hooksShots) {
      if (!wanted(name)) continue;
      await go(p);
      await shot(p, name + v.suffix);
      // Close a list a shot left open before the next page.
      await p.keyboard.press("Escape");
    }
    await p.context().close();
  }
  if (wanted("06-job-hook-failed-dark")) {
    const p = await newPage(browser, { dark: true });
    await signIn(p);
    await openHookedBackup(p, 5);
    await shot(p, "06-job-hook-failed-dark");
    await p.context().close();
  }
  if (wanted("07-job-form-hooks-mobile") && !mobile) {
    const p = await newPage(browser, { width: 390, height: 844 });
    await signIn(p);
    await hooksBackupForm(p);
    await scrollTo(p, p.getByRole("heading", { name: "Hooks" }), 72);
    await shot(p, "07-job-form-hooks-mobile");
    await p.context().close();
  }
}

// bisyncJob opens the newest two-way sync job of the signed-in user that
// matches want: "conflicts" (a run that kept conflicting versions) or an
// error code name such as "RESYNC_REQUIRED". It asks the controller's API
// (same origin), so it needs a real controller, not the in-browser mock.
async function bisyncJob(page, want) {
  await page.goto(`${base}/jobs`);
  await settle(page);
  const id = await page.evaluate(async (want) => {
    const res = await fetch("/syncr.v1.JobService/ListJobs", {
      method: "POST",
      headers: { "Content-Type": "application/json", "X-Syncr-CSRF": "1" },
      body: JSON.stringify({ kinds: ["rclone_bisync"], pageSize: 100 }),
    });
    if (!res.ok) throw new Error(`ListJobs: HTTP ${res.status}`);
    const { jobs = [] } = await res.json();
    const job = jobs.find((j) =>
      want === "conflicts"
        ? Number(j.output?.rcloneBisync?.conflicts ?? 0) > 0
        : j.error?.code === `JOB_ERROR_CODE_${want}`,
    );
    return job?.id;
  }, want);
  if (!id) throw new Error(`no two-way sync job with ${want}`);
  await page.goto(`${base}/jobs/${id}`);
  await settle(page);
}

// bisyncForm opens "Run a job" on the Two-way sync kind for nas-01 and,
// with filled, fills the dev seed's pair: /srv/shared and Cloudflare R2's
// shared folder, with filters, a lower delete limit and check access.
async function bisyncForm(page, filled) {
  await page.goto(`${base}/jobs/new?kind=rclone_bisync`);
  await settle(page);
  await page.getByRole("combobox", { name: /^Worker/ }).click();
  await page.getByRole("option", { name: /nas-01/ }).click();
  if (!filled) return;
  await page.locator("#run-a-path").fill("/srv/shared");
  await page.locator("#run-b-remote").click();
  await page.getByRole("option", { name: /^Cloudflare R2/ }).click();
  await page.locator("#run-b-path").fill("shared");
  await page.locator("#run-bisync-max-delete").fill("30");
  await page.getByRole("switch", { name: "Check access" }).click();
  for (const [i, pattern] of ["*.tmp", ".~lock.*#"].entries()) {
    await page.getByRole("button", { name: "Add filter" }).click();
    await page.getByLabel(`Filter ${i + 1} pattern`).fill(pattern);
  }
  await settle(page, 300);
}

// The two-way sync shots. They expect the dev seed (see the step B
// report): the schedule "Shared folder ⇄ R2" on nas-01 between /srv/shared
// and the remote "Cloudflare R2", with a run that kept two conflicts, runs
// that failed with RESYNC_REQUIRED, SIDE_UNAVAILABLE and (the newest)
// SAFETY_ABORT.
const bisyncShots = [
  [
    "01-editor",
    async (p) => {
      await bisyncForm(p, false);
      await scrollTo(p, p.getByRole("heading", { name: "What kind of job" }));
    },
  ],
  [
    "02-editor-folders",
    async (p) => {
      await bisyncForm(p, true);
      await scrollTo(p, p.getByRole("heading", { name: "Folders" }));
    },
  ],
  [
    "03-editor-options",
    async (p) => {
      await bisyncForm(p, true);
      await scrollTo(p, p.getByRole("heading", { name: "Safety" }));
    },
  ],
  [
    "04-job-conflicts",
    async (p) => {
      await bisyncJob(p, "conflicts");
      await scrollTo(p, p.getByRole("heading", { name: "Two-way sync result" }), 260);
    },
  ],
  [
    "05-job-resync-dialog",
    async (p) => {
      await bisyncJob(p, "RESYNC_REQUIRED");
      await p.getByRole("button", { name: "Resync now" }).click();
      await p.getByRole("alertdialog").waitFor();
      await settle(p, 300);
    },
  ],
  [
    "06-job-safety-abort-dialog",
    async (p) => {
      await bisyncJob(p, "SAFETY_ABORT");
      await p.getByRole("button", { name: "Run once anyway" }).click();
      await p.getByRole("alertdialog").waitFor();
      await settle(p, 300);
    },
  ],
  ["07-job-side-unavailable", async (p) => bisyncJob(p, "SIDE_UNAVAILABLE")],
  [
    "08-schedule",
    async (p) => {
      await openRow(p, "/schedules", "Shared folder ⇄ R2");
      await p.getByText(/^Last run: /).waitFor();
      await settle(p, 300);
    },
  ],
  [
    "09-schedule-runs",
    async (p) => {
      await openRow(p, "/schedules", "Shared folder ⇄ R2");
      await scrollTo(p, p.getByRole("heading", { name: "Run history" }), 96);
    },
  ],
  [
    "10-schedule-form-target",
    async (p) => {
      await p.goto(`${base}/schedules/new?kind=rclone_bisync`);
      await settle(p);
      await p.locator("#sched-name").fill("Shared folder ⇄ R2 (laptop)");
      await scrollTo(p, p.getByRole("heading", { name: "Where it runs" }), 96);
    },
  ],
];

// bisync takes the two-way sync shots, light at 1440×900 (and every shot
// at 390 px with MOBILE=1).
async function bisync(browser) {
  const variants = [{ suffix: "", width: 1440, height: 900 }];
  if (mobile) variants.push({ suffix: "-mobile", width: 390, height: 844 });
  for (const v of variants) {
    const p = await newPage(browser, v);
    await signIn(p);
    for (const [name, go] of bisyncShots) {
      if (!wanted(name)) continue;
      await go(p);
      await shot(p, name + v.suffix);
      // Close a dialog a shot left open before the next page.
      await p.keyboard.press("Escape");
    }
    await p.context().close();
  }
}

// The README shots: one page per feature from the same seeded controller,
// each in light and dark so the README can follow the reader's theme.
const readmeShots = [
  ["dashboard", async (p) => (await p.goto(`${base}/?range=7d`), settle(p, 900))],
  [
    "job-live",
    async (p) => {
      await openRow(p, "/jobs", "Running");
      await p.waitForTimeout(4000);
    },
  ],
  [
    "snapshot-browser",
    async (p) => {
      await openSnapshot(p, "Photos vault", ["srv", "photos", "2026"]);
      for (const n of ["01", "02", "03"])
        await p.getByRole("checkbox", { name: `Select ${n}` }).click();
      await settle(p, 300);
    },
  ],
  ["schedule", async (p) => openRow(p, "/schedules", "Nightly photos")],
  ["workers", async (p) => (await p.goto(`${base}/workers`), settle(p))],
];

// readme takes every README shot at 1440×900, light then dark.
async function readme(browser) {
  for (const v of [
    { suffix: "", dark: false },
    { suffix: "-dark", dark: true },
  ]) {
    const p = await newPage(browser, { dark: v.dark });
    await signIn(p);
    for (const [name, go] of readmeShots) {
      if (!wanted(name)) continue;
      await go(p);
      await shot(p, name + v.suffix);
    }
    await p.context().close();
  }
}

async function main() {
  await mkdir(outDir, { recursive: true });
  const browser = await chromium.launch({ executablePath, args: ["--no-sandbox"] });
  try {
    if (set === "readme") {
      await readme(browser);
      return;
    }
    if (set === "m6") {
      await m6(browser);
      return;
    }
    if (set === "hooks") {
      await hooks(browser);
      return;
    }
    if (set === "bisync") {
      await bisync(browser);
      return;
    }
    if (set === "m5") {
      await m5(browser);
      return;
    }
    if (set === "m4") {
      await m4(browser);
      return;
    }
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
      for (const [name, go] of m3Shots) {
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

// m4 takes the milestone 4 shots: light at 1440×900, then the dashboard in
// dark and at phone width (and every shot at 390 px with MOBILE=1).
async function m4(browser) {
  const variants = [{ suffix: "", width: 1440, height: 900 }];
  if (mobile) variants.push({ suffix: "-mobile", width: 390, height: 844 });
  for (const v of variants) {
    const p = await newPage(browser, v);
    await signIn(p);
    for (const [name, go] of m4Shots) {
      if (!wanted(name)) continue;
      await go(p);
      await shot(p, name + v.suffix);
    }
    await p.context().close();
  }
  if (wanted("02-dashboard-dark")) {
    const p = await newPage(browser, { dark: true });
    await signIn(p);
    await p.goto(`${base}/?range=7d`);
    await settle(p, 900);
    await shot(p, "02-dashboard-dark");
    await p.context().close();
  }
  if (wanted("14-dashboard-mobile") && !mobile) {
    const p = await newPage(browser, { width: 390, height: 844 });
    await signIn(p);
    await p.goto(`${base}/?range=7d`);
    await settle(p, 900);
    await shot(p, "14-dashboard-mobile");
    await p.context().close();
  }
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
