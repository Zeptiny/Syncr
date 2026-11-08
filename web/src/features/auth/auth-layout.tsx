import { ArrowRight, HardDrive } from "lucide-react";

import { Logo, ThemeToggle } from "@/components/app/app-shell";

function RouteSketch() {
  // A quiet picture of what Syncr does: files moving between places.
  const rows = [
    { from: "nas-01:/srv/photos", to: "B2 offsite", pct: 72 },
    { from: "Storage box", to: "backup-host:/srv/backup", pct: 100 },
    { from: "office-pc:D:\\Shared", to: "Nextcloud", pct: 38 },
  ];
  return (
    <div className="flex flex-col gap-3" aria-hidden>
      {rows.map((r) => (
        <div
          key={r.from}
          className="rounded-lg border border-sidebar-border bg-sidebar-accent/70 px-4 py-3"
        >
          <div className="flex items-center gap-2 font-mono text-[12px] text-sidebar-foreground">
            <HardDrive className="size-3.5 opacity-60" />
            <span className="truncate">{r.from}</span>
            <ArrowRight className="size-3.5 shrink-0 opacity-60" />
            <span className="truncate text-sidebar-accent-foreground">{r.to}</span>
          </div>
          <div className="mt-2.5 h-1 overflow-hidden rounded-full bg-sidebar">
            <div
              className={r.pct === 100 ? "h-full bg-sidebar-primary" : "h-full bg-[#6d9dff]"}
              style={{ width: `${r.pct}%` }}
            />
          </div>
        </div>
      ))}
    </div>
  );
}

/** AuthLayout frames the sign-in and setup pages. */
export function AuthLayout({ children, wide }: { children: React.ReactNode; wide?: boolean }) {
  return (
    <div className="grid min-h-svh bg-background lg:grid-cols-[minmax(0,5fr)_minmax(0,7fr)]">
      <aside className="hidden flex-col justify-between bg-sidebar p-10 text-sidebar-foreground lg:flex">
        <div className="flex items-center gap-2.5">
          <Logo />
          <span className="text-[17px] font-semibold text-sidebar-accent-foreground">Syncr</span>
        </div>
        <div className="max-w-md">
          <p className="text-[26px] leading-9 font-semibold tracking-[-0.015em] text-sidebar-accent-foreground">
            Copy, sync and back up files between your machines and your storage.
          </p>
          <p className="mt-3 text-[15px] leading-6">
            Workers run the transfers where the files are. This controller tells them what to do and
            keeps the record.
          </p>
          <div className="mt-8">
            <RouteSketch />
          </div>
        </div>
        <p className="text-xs opacity-60">Self-hosted. Your credentials stay on this controller.</p>
      </aside>
      <main className="relative flex flex-col items-center justify-center px-4 py-12 sm:px-8">
        <div className="absolute top-4 right-4">
          <ThemeToggle />
        </div>
        <div className="mb-8 flex items-center gap-2.5 lg:hidden">
          <Logo />
          <span className="text-[17px] font-semibold">Syncr</span>
        </div>
        <div className={wide ? "w-full max-w-[640px]" : "w-full max-w-[400px]"}>{children}</div>
      </main>
    </div>
  );
}
