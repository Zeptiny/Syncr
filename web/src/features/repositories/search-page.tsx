import { useQuery } from "@connectrpc/connect-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { ChevronRight, Loader2, RefreshCw, Search, SearchX } from "lucide-react";
import { useId, useState } from "react";

import { EmptyState } from "@/components/app/empty-state";
import { EntryIcon } from "@/components/app/entry-icon";
import { ErrorState } from "@/components/app/error-state";
import { WorkerChooser } from "@/components/app/file-browser/worker-chooser";
import { PageHeader } from "@/components/app/page-header";
import { Panel } from "@/components/app/panel";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
import { WorkerAdminService } from "@/gen/syncr/v1/api_pb";
import { ProbeEntryType, type ResticSnapshot } from "@/gen/syncr/v1/link_pb";
import { ResticService } from "@/gen/syncr/v1/restic_pb";
import { fieldViolations } from "@/lib/errors";
import { formatBytes, formatDateTime, tsDate } from "@/lib/format";
import { scanErrorText } from "@/lib/scans";
import { cn } from "@/lib/utils";

import { DeletingBadge } from "./repositories-page";
import { isLocalRepository, type RestoreWhat } from "./model";
import { RestoreSheet, type RestoreSheetProps } from "./restore-sheet";
import { groupMatches, type FoundPath } from "./scans";
import type { FindSearch } from "./search";
import { SnapshotMenu } from "./snapshot-menu";
import { FindNotes, VersionList } from "./version-list";

const PAGE = 100;

/**
 * PatternHelp explains restic find's patterns as restic 0.19 applies them.
 */
export function PatternHelp({ id }: { id?: string }) {
  const code = "rounded bg-muted px-1 font-mono text-[12px]";
  return (
    <div id={id} className="flex flex-col gap-1 text-xs leading-5 text-muted-foreground">
      <p>
        A pattern without <code className={code}>/</code> matches a file or folder of that name
        anywhere in the path. With <code className={code}>/</code> it matches the end of the path (
        <code className={code}>2026/albums.json</code>), or from the top when it starts with{" "}
        <code className={code}>/</code>.
      </p>
      <p>
        <code className={code}>*</code> matches any part of a name, <code className={code}>?</code>{" "}
        one character and <code className={code}>**</code> any number of folders. A matching folder
        also brings everything inside it. To match <code className={code}>*</code>,{" "}
        <code className={code}>?</code> or <code className={code}>[</code> themselves, write{" "}
        <code className={code}>[*]</code>, <code className={code}>[?]</code> or{" "}
        <code className={code}>[[]</code>.
      </p>
    </div>
  );
}

/**
 * SearchPage searches a repository's snapshots with restic find: a
 * pattern, case ignored or not, in every snapshot or one. Matches are
 * grouped by path (how many snapshots have it, its newest size and time);
 * a path opens into its versions, each of which opens its folder in a
 * snapshot or is restored from it. The search is in the URL.
 */
export function SearchPage({
  repositoryId,
  q,
  snapshot,
  icase,
  worker,
}: { repositoryId: string } & FindSearch) {
  const navigate = useNavigate();
  const repoQ = useQuery(ResticService.method.getRepository, { repositoryId });
  const snaps = useQuery(ResticService.method.listSnapshots, {
    repositoryId,
    refresh: false,
    workerId: "",
  });
  const workers = useQuery(WorkerAdminService.method.listWorkers, {});
  const repo = repoQ.data?.repository;
  const local = isLocalRepository(repo);
  const ignoreCase = icase !== false;
  const enabled = !!repo && !!q && (!local || !!worker);
  const find = useQuery(
    ResticService.method.findInRepository,
    {
      repositoryId,
      patterns: q ? [q] : [],
      ignoreCase,
      snapshotIds: snapshot ? [snapshot] : [],
      workerId: worker ?? "",
    },
    { enabled, retry: false, staleTime: Infinity },
  );

  const [typed, setTyped] = useState(q ?? "");
  const [shownQ, setShownQ] = useState(q);
  if (q !== shownQ) {
    setShownQ(q);
    setTyped(q ?? "");
  }
  const [open, setOpen] = useState<ReadonlySet<string>>(new Set());
  const [shown, setShown] = useState(PAGE);
  const [restore, setRestore] = useState<
    { snapshot: RestoreSheetProps["snapshot"]; what: RestoreWhat } | undefined
  >();

  const go = (next: FindSearch) => {
    setOpen(new Set());
    setShown(PAGE);
    void navigate({
      to: "/repositories/$repositoryId/search",
      params: { repositoryId },
      search: next,
    });
  };
  const current: FindSearch = { q, snapshot, icase, worker };

  if (repoQ.error) return <ErrorState error={repoQ.error} onRetry={() => void repoQ.refetch()} />;
  if (!repo)
    return (
      <div className="flex flex-col gap-4">
        <Skeleton className="h-8 w-72" />
        <Skeleton className="h-64 w-full" />
      </div>
    );

  const list = snaps.data?.snapshots ?? [];
  const found = find.data ? groupMatches(find.data.matches, list) : [];
  const patternError = fieldViolations(find.error).find((v) => v.field === "patterns");
  const otherError = find.error && !patternError ? find.error : undefined;
  const openFolder = (id: string, folder: string) =>
    void navigate({
      to: "/repositories/$repositoryId/snapshots/$snapshotId",
      params: { repositoryId, snapshotId: id },
      search: { path: folder === "/" ? undefined : folder, worker },
    });
  const startRestore = (id: string, path: string) => {
    const s = list.find((x) => x.id === id);
    setRestore({
      snapshot: s ?? { id },
      what: { mode: "items", paths: [path] },
    });
  };
  const toggle = (path: string) =>
    setOpen((o) => {
      const n = new Set(o);
      if (n.has(path)) n.delete(path);
      else n.add(path);
      return n;
    });
  const matches = find.data?.matches.length ?? 0;

  return (
    <>
      <PageHeader
        back={{ to: "/repositories", label: "Repositories" }}
        title={
          <span className="flex flex-wrap items-center gap-3">
            Search snapshots
            {repo.deleting && <DeletingBadge />}
          </span>
        }
        meta={
          <Link
            to="/repositories/$repositoryId"
            params={{ repositoryId }}
            className="text-[13px] font-medium text-primary hover:underline"
          >
            {repo.name}
          </Link>
        }
        description="Find files and folders by name in the repository's snapshots, then open or restore any version."
      />
      <div className="flex flex-col gap-6">
        <Panel>
          <form
            role="search"
            aria-label="Search the snapshots"
            className="flex flex-col gap-4"
            onSubmit={(e) => {
              e.preventDefault();
              const t = typed.trim();
              if (!t) return;
              if (t === q) void find.refetch();
              else go({ ...current, q: t });
            }}
          >
            <div className="flex flex-col gap-2">
              <Label htmlFor="find-pattern">Pattern</Label>
              <div className="flex gap-2">
                <Input
                  id="find-pattern"
                  value={typed}
                  onChange={(e) => setTyped(e.target.value)}
                  placeholder="albums.json, *.jpg, 2026/**/*.pdf"
                  spellCheck={false}
                  autoComplete="off"
                  maxLength={256}
                  autoFocus={!q}
                  aria-describedby="find-pattern-help"
                  aria-invalid={!!patternError}
                  className="min-w-0 flex-1 font-mono text-[13px]"
                />
                <Button type="submit" disabled={!typed.trim() || find.isFetching}>
                  {find.isFetching ? (
                    <Loader2 className="animate-spin" aria-hidden />
                  ) : (
                    <Search aria-hidden />
                  )}
                  Search
                </Button>
              </div>
              {patternError && (
                <p role="alert" className="text-xs text-danger">
                  {patternError.message.charAt(0).toUpperCase() + patternError.message.slice(1)}.
                </p>
              )}
              <PatternHelp id="find-pattern-help" />
            </div>
            <div className="flex flex-wrap items-center gap-x-6 gap-y-3">
              <div className="flex items-center gap-2">
                <Switch
                  id="find-icase"
                  checked={ignoreCase}
                  onCheckedChange={(c) => go({ ...current, icase: c ? undefined : false })}
                />
                <Label htmlFor="find-icase" className="font-normal">
                  Ignore case
                </Label>
              </div>
              <div className="flex min-w-0 items-center gap-2">
                <span className="text-[13px] text-muted-foreground">In</span>
                <SnapshotMenu
                  snapshots={list}
                  value={snapshot ?? ""}
                  onChange={(id) => go({ ...current, snapshot: id || undefined })}
                  label="Snapshots searched"
                  allLabel="All snapshots"
                  size="sm"
                  className="min-w-0"
                />
              </div>
              {local && (
                <div className="flex items-center gap-1 text-[13px] text-muted-foreground">
                  Searched
                  <WorkerChooser
                    workers={workers.data?.workers ?? []}
                    op="restic_find"
                    value={worker ?? ""}
                    onChange={(id) => go({ ...current, worker: id || undefined })}
                    required
                    label="Searching worker"
                    menuLabel="Searched by"
                  />
                </div>
              )}
            </div>
          </form>
        </Panel>

        {local && !worker ? (
          <EmptyState
            icon={Search}
            title="Choose a worker first"
            description="This repository is on a worker's disk: only that worker can search it."
            className="py-10"
          />
        ) : !q ? null : find.isFetching ? (
          <div className="flex flex-col gap-3" role="status">
            <span className="text-[13px] text-muted-foreground">
              Searching {snapshot ? "the snapshot" : "every snapshot"}. This can take up to a
              minute.
            </span>
            <Skeleton className="h-14 w-full" />
            <Skeleton className="h-14 w-full" />
            <Skeleton className="h-14 w-full" />
          </div>
        ) : otherError ? (
          <div className="flex flex-col items-start gap-3 rounded-lg border px-5 py-4">
            <p role="alert" className="text-[13px] text-danger">
              {scanErrorText(otherError)}
            </p>
            <Button variant="outline" size="sm" onClick={() => void find.refetch()}>
              <RefreshCw aria-hidden />
              Try again
            </Button>
          </div>
        ) : find.data ? (
          <section aria-label="Search results" className="flex flex-col gap-3">
            <div className="flex flex-wrap items-start justify-between gap-3">
              <FindNotes
                found={
                  matches === 0
                    ? "No matches"
                    : `${matches.toLocaleString("en-US")} ${matches === 1 ? "match" : "matches"}, ${found.length.toLocaleString("en-US")} ${found.length === 1 ? "path" : "different paths"}`
                }
                truncated={find.data.truncated}
                complete={find.data.complete}
                searched={find.data.snapshotsSearched}
                total={snapshot ? 1 : list.length || undefined}
              />
              {find.data.worker?.name && (
                <span className="text-xs text-muted-foreground">
                  Searched by {find.data.worker.name}
                </span>
              )}
            </div>
            {found.length === 0 ? (
              <EmptyState
                icon={SearchX}
                title="Nothing matches"
                description={
                  ignoreCase
                    ? "Check the pattern: without “/” it matches whole names, so add * for part of one (*report*)."
                    : "Check the pattern, or turn on Ignore case."
                }
                className="rounded-lg border py-10"
              />
            ) : (
              <ul className="flex flex-col gap-2">
                {found.slice(0, shown).map((f) => (
                  <FoundRow
                    key={f.path}
                    found={f}
                    snapshots={list}
                    open={open.has(f.path)}
                    onToggle={() => toggle(f.path)}
                    onOpen={openFolder}
                    onRestore={startRestore}
                    locked={repo.deleting}
                  />
                ))}
              </ul>
            )}
            {found.length > shown && (
              <div className="flex items-center justify-between gap-3 text-[13px] text-muted-foreground">
                <span className="tabular">
                  Showing {shown.toLocaleString("en-US")} of {found.length.toLocaleString("en-US")}{" "}
                  paths
                </span>
                <Button variant="outline" size="sm" onClick={() => setShown((n) => n + PAGE)}>
                  Show {Math.min(PAGE, found.length - shown)} more
                </Button>
              </div>
            )}
          </section>
        ) : null}
      </div>

      {restore && (
        <RestoreSheet
          open
          onOpenChange={(o) => !o && setRestore(undefined)}
          repository={repo}
          snapshot={restore.snapshot}
          what={restore.what}
          workers={workers.data?.workers ?? []}
        />
      )}
    </>
  );
}

/** FoundRow is one path of the results, opening into its versions. */
function FoundRow({
  found,
  snapshots,
  open,
  onToggle,
  onOpen,
  onRestore,
  locked,
}: {
  found: FoundPath;
  snapshots: ResticSnapshot[];
  open: boolean;
  onToggle: () => void;
  onOpen: (snapshotId: string, folder: string) => void;
  onRestore: (snapshotId: string, path: string) => void;
  locked: boolean;
}) {
  const e = found.entry;
  const n = found.snapshotIds.length;
  const v = found.versions.length;
  const dir = e.type === ProbeEntryType.DIR;
  const panel = useId();
  return (
    <li className="rounded-lg border bg-card">
      <button
        type="button"
        aria-expanded={open}
        aria-controls={panel}
        onClick={onToggle}
        className="flex w-full items-start gap-3 rounded-lg px-4 py-3 text-left hover:bg-accent/50 focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
      >
        <ChevronRight
          className={cn(
            "mt-0.5 size-4 shrink-0 text-muted-foreground transition-transform",
            open && "rotate-90",
          )}
          aria-hidden
        />
        <EntryIcon type={e.type} />
        <span className="flex min-w-0 flex-1 flex-col gap-0.5">
          <span className="font-mono text-[13px] break-all">{found.path}</span>
          <span className="text-xs text-muted-foreground">
            In {n === 1 ? "1 snapshot" : `${n.toLocaleString("en-US")} snapshots`}
            {!dir && `, ${v === 1 ? "1 version" : `${v} versions`}`}
            {!dir && (
              <>
                {" "}
                · newest {formatBytes(e.size)}
                {e.modTime && <>, modified {formatDateTime(tsDate(e.modTime))}</>}
              </>
            )}
          </span>
        </span>
      </button>
      {open && (
        <div id={panel} className="border-t px-4 py-3">
          <VersionList
            found={found}
            snapshots={snapshots}
            onOpen={onOpen}
            onRestore={onRestore}
            locked={locked}
          />
        </div>
      )}
    </li>
  );
}
