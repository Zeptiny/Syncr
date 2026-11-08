import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { useQuery } from "@connectrpc/connect-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { ArrowRightLeft, ChevronDown, Play } from "lucide-react";
import { useCallback, useMemo } from "react";

import { EmptyState } from "@/components/app/empty-state";
import { FilterBar, SearchInput } from "@/components/app/filter-bar";
import { PageHeader } from "@/components/app/page-header";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectSeparator,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { JobService, JobStatus, WorkerAdminService } from "@/gen/syncr/v1/api_pb";
import { RemoteService } from "@/gen/syncr/v1/remotes_pb";
import { UserService } from "@/gen/syncr/v1/users_pb";
import { useKeysetPager } from "@/hooks/use-keyset-pager";
import { useNow } from "@/hooks/use-now";
import { useSession } from "@/hooks/use-session";
import { isTerminal, jobStatuses, jobStatusMeta, kindLabel } from "@/lib/status";

import { JobsTable } from "./jobs-table";

/** JobsSearch is the jobs list's URL state. */
export interface JobsSearch {
  /** status is a comma-separated list of JobStatus numbers. */
  status?: string;
  worker?: string;
  remote?: string;
  kind?: string;
  q?: string;
  period?: "24h" | "7d" | "30d" | "custom";
  from?: string;
  to?: string;
  all?: boolean;
}

const periods = { "24h": 1, "7d": 7, "30d": 30 } as const;
const kinds = ["rclone_transfer"];
const ANY = "__any__";

/** validateJobsSearch parses the URL search params of the jobs list. */
export function validateJobsSearch(s: Record<string, unknown>): JobsSearch {
  const str = (v: unknown) => (typeof v === "string" && v ? v : undefined);
  const date = (v: unknown) =>
    typeof v === "string" && /^\d{4}-\d{2}-\d{2}$/.test(v) ? v : undefined;
  const period = str(s.period);
  return {
    status: str(s.status),
    worker: str(s.worker),
    remote: str(s.remote),
    kind: str(s.kind),
    q: str(s.q),
    period:
      period === "24h" || period === "7d" || period === "30d" || period === "custom"
        ? period
        : undefined,
    from: date(s.from),
    to: date(s.to),
    all: s.all === true || s.all === "true" ? true : undefined,
  };
}

function parseStatuses(s: string | undefined): JobStatus[] {
  if (!s) return [];
  return s
    .split(",")
    .map(Number)
    .filter((n) => jobStatuses.includes(n));
}

function range(search: JobsSearch, now: number): { after?: Date; before?: Date } {
  if (search.period && search.period !== "custom") {
    return { after: new Date(now - periods[search.period] * 86_400_000) };
  }
  if (search.period === "custom") {
    const after = search.from ? new Date(`${search.from}T00:00:00`) : undefined;
    const before = search.to ? new Date(`${search.to}T23:59:59.999`) : undefined;
    return { after, before };
  }
  return {};
}

function StatusFilter({
  value,
  onChange,
}: {
  value: JobStatus[];
  onChange: (v: JobStatus[]) => void;
}) {
  const label =
    value.length === 0
      ? "Any status"
      : value.length === 1
        ? jobStatusMeta(value[0]).label
        : `${value.length} statuses`;
  return (
    <Popover>
      <PopoverTrigger asChild>
        <Button
          variant="outline"
          className="w-[calc(50%-0.25rem)] justify-between bg-card font-normal sm:w-40"
        >
          {label}
          <ChevronDown className="opacity-50" aria-hidden />
        </Button>
      </PopoverTrigger>
      <PopoverContent align="start" className="w-52 p-1.5">
        {jobStatuses.map((s) => {
          const id = `status-${s}`;
          const checked = value.includes(s);
          return (
            <Label
              key={s}
              htmlFor={id}
              className="flex cursor-pointer items-center gap-2.5 rounded-md px-2 py-1.5 font-normal hover:bg-accent"
            >
              <Checkbox
                id={id}
                checked={checked}
                onCheckedChange={(c) => onChange(c ? [...value, s] : value.filter((x) => x !== s))}
              />
              {jobStatusMeta(s).label}
            </Label>
          );
        })}
      </PopoverContent>
    </Popover>
  );
}

/** JobsPage lists jobs with filters, search and a keyset pager. */
export function JobsPage({ search }: { search: JobsSearch }) {
  const navigate = useNavigate();
  const { isAdmin } = useSession();
  const setSearch = useCallback(
    (patch: Partial<JobsSearch>) =>
      void navigate({ to: "/jobs", search: (prev) => ({ ...prev, ...patch }), replace: true }),
    [navigate],
  );
  const statuses = parseStatuses(search.status);
  const allUsers = isAdmin && !!search.all;
  const filterKey = JSON.stringify(search);
  // Relative periods move once a minute, so the query key stays stable.
  const minute = useNow(60_000).getTime();
  const { after, before } = useMemo(() => range(search, minute), [search, minute]);
  const pager = useKeysetPager(filterKey);

  const workers = useQuery(WorkerAdminService.method.listWorkers, {});
  const remotes = useQuery(RemoteService.method.listRemotes, {});
  const users = useQuery(UserService.method.listUsers, {}, { enabled: allUsers });
  const q = useQuery(
    JobService.method.listJobs,
    {
      statuses,
      workerId: search.worker ?? "",
      remoteId: search.remote ?? "",
      kinds: search.kind ? [search.kind] : [],
      query: search.q ?? "",
      createdAfter: after ? timestampFromDate(after) : undefined,
      createdBefore: before ? timestampFromDate(before) : undefined,
      allUsers,
      pageSize: 25,
      pageToken: pager.token,
    },
    {
      placeholderData: (prev) => prev,
      refetchInterval: (query) =>
        query.state.data?.jobs.some((j) => !isTerminal(j.status)) ? 5000 : 30_000,
    },
  );

  const workerMap = useMemo(
    () => new Map((workers.data?.workers ?? []).map((w) => [w.id, w])),
    [workers.data],
  );
  const owners = useMemo(
    () => (allUsers ? new Map((users.data?.users ?? []).map((u) => [u.id, u])) : undefined),
    [allUsers, users.data],
  );
  const filtered =
    statuses.length > 0 ||
    !!search.worker ||
    !!search.remote ||
    !!search.kind ||
    !!search.q ||
    !!search.period;

  return (
    <>
      <PageHeader
        title="Jobs"
        description="Every transfer, newest first. Open one to follow it live."
        actions={
          <Button asChild>
            <Link to="/jobs/new">
              <Play aria-hidden />
              Run a job
            </Link>
          </Button>
        }
      />
      <FilterBar
        active={filtered}
        onClear={() => void navigate({ to: "/jobs", search: { all: search.all }, replace: true })}
      >
        <SearchInput
          value={search.q ?? ""}
          onChange={(v) => setSearch({ q: v || undefined })}
          placeholder="Search paths and remotes"
        />
        <StatusFilter
          value={statuses}
          onChange={(v) => setSearch({ status: v.length ? [...v].sort().join(",") : undefined })}
        />
        <Select
          value={search.worker ?? ANY}
          onValueChange={(v) => setSearch({ worker: v === ANY ? undefined : v })}
        >
          <SelectTrigger className="w-[calc(50%-0.25rem)] bg-card sm:w-40" aria-label="Worker">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value={ANY}>Any worker</SelectItem>
            <SelectSeparator />
            {(workers.data?.workers ?? []).map((w) => (
              <SelectItem key={w.id} value={w.id}>
                {w.name}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Select
          value={search.remote ?? ANY}
          onValueChange={(v) => setSearch({ remote: v === ANY ? undefined : v })}
        >
          <SelectTrigger className="w-[calc(50%-0.25rem)] bg-card sm:w-40" aria-label="Remote">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value={ANY}>Any remote</SelectItem>
            <SelectSeparator />
            {(remotes.data?.remotes ?? []).map((r) => (
              <SelectItem key={r.id} value={r.id}>
                {r.name}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Select
          value={search.kind ?? ANY}
          onValueChange={(v) => setSearch({ kind: v === ANY ? undefined : v })}
        >
          <SelectTrigger className="w-[calc(50%-0.25rem)] bg-card sm:w-36" aria-label="Kind">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value={ANY}>Any kind</SelectItem>
            <SelectSeparator />
            {kinds.map((k) => (
              <SelectItem key={k} value={k}>
                {kindLabel(k)}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Select
          value={search.period ?? ANY}
          onValueChange={(v) =>
            setSearch({
              period: v === ANY ? undefined : (v as JobsSearch["period"]),
              from: undefined,
              to: undefined,
            })
          }
        >
          <SelectTrigger className="w-[calc(50%-0.25rem)] bg-card sm:w-40" aria-label="Created">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value={ANY}>Any time</SelectItem>
            <SelectSeparator />
            <SelectItem value="24h">Last 24 hours</SelectItem>
            <SelectItem value="7d">Last 7 days</SelectItem>
            <SelectItem value="30d">Last 30 days</SelectItem>
            <SelectItem value="custom">Custom range</SelectItem>
          </SelectContent>
        </Select>
        {search.period === "custom" && (
          <div className="flex items-center gap-1.5">
            <Input
              type="date"
              aria-label="Created from"
              className="w-40 bg-card"
              value={search.from ?? ""}
              max={search.to}
              onChange={(e) => setSearch({ from: e.target.value || undefined })}
            />
            <span className="text-muted-foreground">to</span>
            <Input
              type="date"
              aria-label="Created until"
              className="w-40 bg-card"
              value={search.to ?? ""}
              min={search.from}
              onChange={(e) => setSearch({ to: e.target.value || undefined })}
            />
          </div>
        )}
        {isAdmin && (
          <Label className="ml-auto flex items-center gap-2 text-[13px] font-normal">
            <Switch
              checked={!!search.all}
              onCheckedChange={(c) => setSearch({ all: c || undefined })}
            />
            All users
          </Label>
        )}
      </FilterBar>
      <JobsTable
        jobs={q.data?.jobs}
        isLoading={q.isPending}
        error={q.error ?? undefined}
        onRetry={() => void q.refetch()}
        workers={workerMap}
        owners={owners}
        pager={{
          page: pager.page,
          canPrev: pager.canPrev,
          canNext: !!q.data?.nextPageToken,
          onPrev: pager.prev,
          onNext: () => pager.next(q.data?.nextPageToken ?? ""),
        }}
        empty={
          filtered ? (
            <EmptyState
              icon={ArrowRightLeft}
              title="No jobs match these filters"
              description="Widen the time range or clear the filters to see more."
            />
          ) : (
            <EmptyState
              icon={ArrowRightLeft}
              title="No jobs yet"
              description="A job copies, syncs or moves files from a source to a destination on one of your workers."
              action={
                <Button asChild>
                  <Link to="/jobs/new">Run your first job</Link>
                </Button>
              }
            />
          )
        }
      />
    </>
  );
}
