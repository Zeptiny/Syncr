import { useQuery } from "@connectrpc/connect-query";
import { Link, useNavigate } from "@tanstack/react-router";
import {
  ArrowRightLeft,
  CalendarClock,
  GitCompareArrows,
  Pencil,
  Play,
  Plus,
  Scissors,
  Search,
  ShieldCheck,
  Trash2,
} from "lucide-react";
import { Code } from "@connectrpc/connect";
import { useEffect, useMemo, useRef, useState } from "react";
import { toast } from "sonner";

import { EmptyState } from "@/components/app/empty-state";
import { ErrorState } from "@/components/app/error-state";
import { PageHeader } from "@/components/app/page-header";
import { Panel } from "@/components/app/panel";
import { RelativeTime } from "@/components/app/relative-time";
import { ShortId } from "@/components/app/short-id";
import { StatCard, StatGrid } from "@/components/app/stat-card";
import { StatusBadge, StatusPill } from "@/components/app/status-badge";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { JobService, JobStatus, WorkerAdminService } from "@/gen/syncr/v1/api_pb";
import { ResticService, type Repository } from "@/gen/syncr/v1/restic_pb";
import { ScheduleService } from "@/gen/syncr/v1/schedules_pb";
import { useKeysetPager } from "@/hooks/use-keyset-pager";
import { cronText } from "@/lib/cron";
import { hasCode } from "@/lib/errors";
import { formatBytes, tsDate } from "@/lib/format";
import { isTerminal, kindLabel } from "@/lib/status";

import { JobsTable } from "../jobs/jobs-table";
import { DeleteRepositoryDialog } from "./delete-repository-dialog";
import { DeletionBanner } from "./deletion-banner";
import { DeletingBadge, RepositoryLocation } from "./repositories-page";
import { RepositorySettings } from "./repository-settings";
import { runScan } from "./scan-run";
import type { RepositoryTab } from "./search";
import { SnapshotsTable } from "./snapshots-table";

const ACTIVE = [JobStatus.QUEUED, JobStatus.DISPATCHED, JobStatus.RUNNING];

/** ActionLink is a header action: a link, or a disabled button while the data is deleted. */
function ActionLink({
  kind,
  repo,
  icon: Icon,
  children,
}: {
  kind: "restic_backup" | "restic_check" | "restic_forget";
  repo: Repository;
  icon: typeof Play;
  children: React.ReactNode;
}) {
  if (repo.deleting)
    return (
      <Button variant="outline" disabled>
        <Icon aria-hidden />
        {children}
      </Button>
    );
  return (
    <Button variant="outline" asChild>
      <Link to="/jobs/new" search={{ kind, repository: repo.id }}>
        <Icon aria-hidden />
        {children}
      </Link>
    </Button>
  );
}

/**
 * RepositoryDetailPage shows one repository: its figures, snapshots, jobs,
 * schedules and settings. While its data is being deleted every action is
 * off except viewing and cancelling the deletion; the tabs stay readable.
 */
export function RepositoryDetailPage({
  repositoryId,
  tab = "snapshots",
}: {
  repositoryId: string;
  tab?: RepositoryTab;
}) {
  const navigate = useNavigate();
  const q = useQuery(
    ResticService.method.getRepository,
    { repositoryId },
    { refetchInterval: (query) => (query.state.data?.repository?.deleting ? 4000 : false) },
  );
  const workers = useQuery(WorkerAdminService.method.listWorkers, {});
  const schedules = useQuery(ScheduleService.method.listSchedules, { repositoryId });
  const active = useQuery(
    JobService.method.listJobs,
    { repositoryId, statuses: ACTIVE, pageSize: 50 },
    { refetchInterval: 5000 },
  );
  const pager = useKeysetPager(repositoryId);
  const jobs = useQuery(
    JobService.method.listJobs,
    { repositoryId, pageSize: 25, pageToken: pager.token },
    {
      enabled: tab === "jobs",
      placeholderData: (prev) => prev,
      refetchInterval: (query) =>
        query.state.data?.jobs.some((j) => !isTerminal(j.status)) ? 5000 : 30_000,
    },
  );
  const [deleting, setDeleting] = useState<"forget" | "data" | undefined>();
  const workerList = useMemo(() => workers.data?.workers ?? [], [workers.data]);
  const workerMap = useMemo(() => new Map(workerList.map((w) => [w.id, w])), [workerList]);

  // A repository whose data was being deleted disappears when the deletion
  // succeeds: say so and go back to the list instead of showing "not found".
  const deletingName = useRef<string | undefined>(undefined);
  const current = q.data?.repository;
  const gone = !!q.error && hasCode(q.error, Code.NotFound);
  useEffect(() => {
    if (current?.deleting && !gone) deletingName.current = current.name;
    else if (current && !gone) deletingName.current = undefined;
    if (gone && deletingName.current) {
      toast.success(`Deleted ${deletingName.current} and its data`);
      deletingName.current = undefined;
      void navigate({ to: "/repositories" });
    }
  }, [current, gone, navigate]);

  if (q.error) return <ErrorState error={q.error} onRetry={() => void q.refetch()} />;
  const repo = q.data?.repository;
  if (!repo) {
    return (
      <div className="flex flex-col gap-4">
        <Skeleton className="h-8 w-72" />
        <Skeleton className="h-24 w-full" />
        <Skeleton className="h-96 w-full" />
      </div>
    );
  }
  const s = repo.stats;
  const scheduleList = schedules.data?.schedules ?? [];
  const others = (active.data?.jobs ?? []).filter((j) => j.id !== repo.deletionJobId);
  const setTab = (t: string) =>
    void navigate({
      to: "/repositories/$repositoryId",
      params: { repositoryId },
      search: { tab: t === "snapshots" ? undefined : (t as RepositoryTab) },
      replace: true,
    });

  return (
    <>
      <PageHeader
        back={{ to: "/repositories", label: "Repositories" }}
        title={
          <span className="flex flex-wrap items-center gap-3">
            {repo.name}
            {repo.deleting && <DeletingBadge />}
          </span>
        }
        meta={
          <>
            <RepositoryLocation repo={repo} />
            {repo.resticId && (
              <span className="inline-flex items-center gap-1 text-xs text-muted-foreground">
                restic ID <ShortId id={repo.resticId} copy />
              </span>
            )}
            {repo.description && (
              <span className="text-[13px] text-muted-foreground">{repo.description}</span>
            )}
          </>
        }
        actions={
          <>
            <Button variant="outline" asChild>
              <Link to="/repositories/$repositoryId/search" params={{ repositoryId: repo.id }}>
                <Search aria-hidden />
                Search
              </Link>
            </Button>
            <ActionLink kind="restic_check" repo={repo} icon={ShieldCheck}>
              Check now
            </ActionLink>
            <ActionLink kind="restic_forget" repo={repo} icon={Scissors}>
              Retention…
            </ActionLink>
            <Button variant="outline" disabled={repo.deleting} onClick={() => setTab("settings")}>
              <Pencil aria-hidden />
              Edit
            </Button>
            <Button
              variant="outline"
              className="text-danger hover:text-danger"
              disabled={repo.deleting}
              onClick={() => setDeleting("forget")}
            >
              <Trash2 aria-hidden />
              Delete
            </Button>
            {repo.deleting ? (
              <Button disabled>
                <Play aria-hidden />
                Back up now
              </Button>
            ) : (
              <Button asChild>
                <Link to="/jobs/new" search={{ kind: "restic_backup", repository: repo.id }}>
                  <Play aria-hidden />
                  Back up now
                </Link>
              </Button>
            )}
          </>
        }
      />

      <div className="flex flex-col gap-6">
        <DeletionBanner
          repository={repo}
          workers={workerList}
          waitingFor={others.length}
          pausedSchedules={scheduleList.filter(
            (x) => !x.enabled && x.pausedReason === "repository_deleting",
          )}
          onTryAgain={() => setDeleting("data")}
          onForget={() => setDeleting("forget")}
        />

        <StatGrid className="md:grid-cols-4">
          <StatCard
            label="Snapshots"
            value={`${s?.snapshotCount ?? 0}${s?.snapshotsTruncated ? "+" : ""}`}
            sub={
              s?.snapshotsRefreshedAt ? (
                <>
                  listed <RelativeTime date={tsDate(s.snapshotsRefreshedAt)} />
                  {s.snapshotsStale ? ", out of date" : ""}
                </>
              ) : (
                "not listed yet"
              )
            }
          />
          <StatCard
            label="Latest snapshot"
            value={<RelativeTime date={tsDate(s?.latestSnapshotAt)} empty="None" />}
            sub={
              s?.lastBackup ? (
                <span className="inline-flex items-center gap-1.5">
                  last backup <StatusBadge job={s.lastBackup.status} className="h-5 px-2" />
                </span>
              ) : (
                "no backup run yet"
              )
            }
          />
          <StatCard
            label="Latest size"
            value={s?.latestSnapshotAt ? formatBytes(s.latestSnapshotBytes) : "–"}
            sub="processed by the latest backup"
          />
          <StatCard
            label="Last check"
            value={
              s?.lastCheck ? (
                <RelativeTime date={tsDate(s.lastCheck.finishedAt ?? s.lastCheck.createdAt)} />
              ) : (
                "Never"
              )
            }
            tone={s?.lastCheck?.status === JobStatus.FAILED ? "danger" : "default"}
            sub={
              s?.lastCheck
                ? s.lastCheck.status === JobStatus.SUCCEEDED
                  ? "no errors found"
                  : s.lastCheck.status === JobStatus.FAILED
                    ? "found errors or failed"
                    : "in progress"
                : "run one to verify the backups"
            }
          />
        </StatGrid>

        <Tabs value={tab} onValueChange={setTab}>
          <TabsList>
            <TabsTrigger value="snapshots">Snapshots</TabsTrigger>
            <TabsTrigger value="jobs">Jobs</TabsTrigger>
            <TabsTrigger value="schedules">
              Schedules{scheduleList.length ? ` (${scheduleList.length})` : ""}
            </TabsTrigger>
            <TabsTrigger value="settings">Settings</TabsTrigger>
          </TabsList>

          <TabsContent value="snapshots" className="pt-4">
            <SnapshotsTable
              repository={repo}
              workers={workerList}
              rowActions={(snap, previous) =>
                previous ? (
                  <Button variant="ghost" size="icon-sm" asChild>
                    <Link
                      to="/repositories/$repositoryId/compare"
                      params={{ repositoryId: repo.id }}
                      search={{ from: previous.id, to: snap.id }}
                      state={runScan}
                      aria-label={`Compare ${snap.id.slice(0, 8)} with previous`}
                      title="Compare with the previous snapshot of the same host and paths"
                    >
                      <GitCompareArrows aria-hidden />
                    </Link>
                  </Button>
                ) : null
              }
            />
          </TabsContent>

          <TabsContent value="jobs" className="pt-4">
            <div className="mb-3 flex justify-end">
              <Button variant="ghost" size="sm" asChild>
                <Link to="/jobs" search={{ repository: repo.id }}>
                  <ArrowRightLeft aria-hidden />
                  Open in Jobs with filters
                </Link>
              </Button>
            </div>
            <JobsTable
              jobs={jobs.data?.jobs}
              isLoading={jobs.isPending}
              error={jobs.error ?? undefined}
              onRetry={() => void jobs.refetch()}
              workers={workerMap}
              pager={{
                page: pager.page,
                canPrev: pager.canPrev,
                canNext: !!jobs.data?.nextPageToken,
                onPrev: pager.prev,
                onNext: () => pager.next(jobs.data?.nextPageToken ?? ""),
              }}
              empty={
                <EmptyState
                  icon={ArrowRightLeft}
                  title="No jobs on this repository yet"
                  description="Backups, checks, retention runs and restores of this repository show up here."
                />
              }
            />
          </TabsContent>

          <TabsContent value="schedules" className="pt-4">
            <Panel
              flush
              title="Schedules using this repository"
              actions={
                <div className="flex flex-wrap gap-2">
                  {(["restic_backup", "restic_forget", "restic_check"] as const).map((kind) =>
                    repo.deleting ? (
                      <Button key={kind} variant="outline" size="sm" disabled>
                        <Plus aria-hidden />
                        {kindLabel(kind)}
                      </Button>
                    ) : (
                      <Button key={kind} variant="outline" size="sm" asChild>
                        <Link to="/schedules/new" search={{ kind, repository: repo.id }}>
                          <Plus aria-hidden />
                          {kindLabel(kind)}
                        </Link>
                      </Button>
                    ),
                  )}
                </div>
              }
            >
              {schedules.error ? (
                <ErrorState error={schedules.error} onRetry={() => void schedules.refetch()} />
              ) : scheduleList.length === 0 ? (
                <EmptyState
                  icon={CalendarClock}
                  title="No schedules yet"
                  description="Schedule a nightly backup, a weekly retention run and a monthly check with the buttons above."
                />
              ) : (
                <ul className="divide-y">
                  {scheduleList.map((sc) => (
                    <li key={sc.id}>
                      <Link
                        to="/schedules/$scheduleId"
                        params={{ scheduleId: sc.id }}
                        className="flex flex-wrap items-center justify-between gap-x-4 gap-y-1 px-5 py-3 hover:bg-accent/50"
                      >
                        <span className="flex min-w-0 flex-col">
                          <span className="truncate font-medium">{sc.name}</span>
                          <span className="text-xs text-muted-foreground">
                            {kindLabel(kindOf(sc.definition?.operation.case))}, {cronText(sc.cron)}
                          </span>
                        </span>
                        {sc.enabled ? (
                          <span className="text-xs whitespace-nowrap text-muted-foreground">
                            next <RelativeTime date={tsDate(sc.nextRunAt)} empty="not planned" />
                          </span>
                        ) : (
                          <StatusPill
                            meta={{
                              label: sc.pausedReason ? "Paused automatically" : "Paused",
                              tone: sc.pausedReason ? "warning" : "neutral",
                            }}
                          />
                        )}
                      </Link>
                    </li>
                  ))}
                </ul>
              )}
            </Panel>
          </TabsContent>

          <TabsContent value="settings" className="pt-4">
            <RepositorySettings
              key={String(repo.version)}
              repository={repo}
              workers={workerList}
              onDelete={() => setDeleting("forget")}
            />
          </TabsContent>
        </Tabs>
      </div>

      <DeleteRepositoryDialog
        open={deleting !== undefined}
        onOpenChange={(o) => !o && setDeleting(undefined)}
        repository={repo}
        schedules={scheduleList}
        workers={workerList}
        activeJobs={others.length}
        initialDeleteData={deleting === "data"}
        onDone={(res, deleteData) => {
          if (deleteData) {
            toast.success(
              `Deleting the data of ${repo.name}${res.worker?.name ? ` on ${res.worker.name}` : ""}`,
            );
          } else {
            toast.success(`Forgot ${repo.name}`);
            void navigate({ to: "/repositories" });
          }
        }}
      />
    </>
  );
}

/** kindOf maps a definition's oneof case to its job kind name. */
function kindOf(c: string | undefined): string {
  switch (c) {
    case "resticBackup":
      return "restic_backup";
    case "resticForget":
      return "restic_forget";
    case "resticPrune":
      return "restic_prune";
    case "resticCheck":
      return "restic_check";
    case "resticRestore":
      return "restic_restore";
    default:
      return "rclone_transfer";
  }
}
