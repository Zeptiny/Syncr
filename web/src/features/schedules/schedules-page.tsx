import { useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { createColumnHelper } from "@tanstack/react-table";
import {
  AlertTriangle,
  ArrowDown,
  ArrowUp,
  ArrowUpDown,
  CalendarClock,
  ChevronRight,
  FolderInput,
  FolderPlus,
  MoreHorizontal,
  Pause,
  Pencil,
  Play,
  Plus,
  Tag,
  Trash2,
  X,
} from "lucide-react";
import { createContext, useCallback, useContext, useMemo, useState } from "react";

import { ConfirmDialog } from "@/components/app/confirm-dialog";
import { DataTable, type DataTableSection } from "@/components/app/data-table";
import { EmptyState } from "@/components/app/empty-state";
import { FilterBar, SearchInput } from "@/components/app/filter-bar";
import { PageHeader } from "@/components/app/page-header";
import { HelpLink } from "@/features/docs/doc-link";
import { RelativeTime } from "@/components/app/relative-time";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuSub,
  DropdownMenuSubContent,
  DropdownMenuSubTrigger,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectSeparator,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { WorkerAdminService, type Worker } from "@/gen/syncr/v1/api_pb";
import { ScheduleService, type Schedule, type ScheduleGroup } from "@/gen/syncr/v1/schedules_pb";
import { cronText } from "@/lib/cron";
import { tsDate } from "@/lib/format";
import { kindLabel, workerStatusMeta } from "@/lib/status";
import { formatInZone } from "@/lib/timezones";
import { toneDot } from "@/lib/tones";
import { cn } from "@/lib/utils";
import { invalidateService } from "@/lib/invalidate";
import { toast } from "sonner";

import { schedulesText, useBulkScheduleActions, useScheduleActions } from "./actions";
import { GroupNameDialog, useScheduleGroups } from "./groups";
import {
  counts,
  filtered,
  matches,
  nextSort,
  sections,
  sortSchedules,
  statusTitles,
  targetKey,
  targetTitle,
  type Section,
} from "./list";
import { kindOf, targetLabel } from "./model";
import { RunOutcome } from "./run-outcome";
import type { GroupBy, ScheduleSort, SchedulesSearch, StatusFilter } from "./search";
import { ScheduleWhat } from "./summary";

const col = createColumnHelper<Schedule>();
const ANY = "__any__";

/** TargetChip shows where a schedule runs: a worker with its status, or a label. */
export function TargetChip({
  schedule,
  workers,
}: {
  schedule: Schedule;
  workers: Map<string, Worker>;
}) {
  const t = targetLabel(schedule.target, workers);
  if (t.kind === "label") {
    return (
      <span className="inline-flex items-center gap-1 rounded bg-secondary px-1.5 py-0.5 font-mono text-[12px] whitespace-nowrap text-secondary-foreground">
        <Tag className="size-3 opacity-60" aria-hidden />
        {t.text}
      </span>
    );
  }
  const meta = t.worker ? workerStatusMeta(t.worker.status) : undefined;
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1.5 text-[13px] whitespace-nowrap",
        !t.worker && "text-muted-foreground line-through",
      )}
    >
      {meta && (
        <span className={cn("size-1.5 rounded-full", toneDot[meta.tone])} aria-label={meta.label} />
      )}
      {t.text}
    </span>
  );
}

/** NextRun shows when a schedule runs next, or that it is paused. */
export function NextRun({ schedule }: { schedule: Schedule }) {
  if (!schedule.enabled) {
    return (
      <span className="text-[13px] text-muted-foreground">
        {schedule.pausedReason ? "Paused automatically" : "Paused"}
      </span>
    );
  }
  const at = tsDate(schedule.nextRunAt);
  if (!at) return <span className="text-[13px] text-muted-foreground">Never</span>;
  return (
    <div className="flex flex-col">
      <RelativeTime date={at} className="text-[13px] font-medium" />
      <span className="tabular text-xs whitespace-nowrap text-muted-foreground">
        {formatInZone(at, schedule.timezone, false)}
      </span>
    </div>
  );
}

/** stop keeps a click or key in a cell's control from opening the row. */
function Stop({ children }: { children: React.ReactNode }) {
  return (
    <span onClick={(e) => e.stopPropagation()} onKeyDown={(e) => e.stopPropagation()}>
      {children}
    </span>
  );
}

/** SortHeader is a column header that sorts the list by key. */
function SortHeader({
  label,
  sortKey,
  sort,
  onSort,
}: {
  label: string;
  sortKey: "name" | "next" | "last";
  sort: ScheduleSort | undefined;
  onSort: (s: ScheduleSort) => void;
}) {
  const cur = sort ?? "name";
  const active = cur.replace(/^-/, "") === sortKey;
  const Icon = !active ? ArrowUpDown : cur.startsWith("-") ? ArrowDown : ArrowUp;
  return (
    <button
      type="button"
      onClick={() => onSort(nextSort(sort, sortKey))}
      className={cn(
        "-mx-1 inline-flex items-center gap-1 rounded px-1 hover:text-foreground",
        active && "text-foreground",
      )}
    >
      {label}
      <Icon className={cn("size-3", !active && "opacity-50")} aria-hidden />
    </button>
  );
}

/** ariaSort is a column's aria-sort under a sort: undefined for columns that do not sort. */
function ariaSort(sort: ScheduleSort | undefined, columnId: string) {
  if (!Object.values(sortColumn).includes(columnId)) return undefined;
  const cur = sort ?? "name";
  if (sortColumn[cur.replace(/^-/, "")] !== columnId) return "none" as const;
  return cur.startsWith("-") ? ("descending" as const) : ("ascending" as const);
}

// The collapsed sections, per grouping, are remembered in the browser.
const COLLAPSED_KEY = "syncr-schedule-sections";

function loadCollapsed(): Record<string, string[]> {
  try {
    const v: unknown = JSON.parse(window.localStorage.getItem(COLLAPSED_KEY) ?? "{}");
    if (!v || typeof v !== "object" || Array.isArray(v)) return {};
    // Only lists of section keys are kept: anything else stored there is dropped.
    const out: Record<string, string[]> = {};
    for (const [by, keys] of Object.entries(v as Record<string, unknown>)) {
      if (Array.isArray(keys))
        out[by] = keys.filter((k: unknown): k is string => typeof k === "string");
    }
    return out;
  } catch {
    return {};
  }
}

function useCollapsed(by: GroupBy): [Set<string>, (key: string, collapsed: boolean) => void] {
  const [all, setAll] = useState(loadCollapsed);
  const set = useMemo(() => new Set(all[by] ?? []), [all, by]);
  const toggle = useCallback(
    (key: string, collapsed: boolean) => {
      setAll((prev) => {
        const keys = new Set(prev[by] ?? []);
        if (collapsed) keys.add(key);
        else keys.delete(key);
        const next = { ...prev, [by]: [...keys] };
        try {
          window.localStorage.setItem(COLLAPSED_KEY, JSON.stringify(next));
        } catch {
          // A full or blocked storage only forgets the state.
        }
        return next;
      });
    },
    [by],
  );
  return [set, toggle];
}

/** selectionState is a checkbox's state for a set of schedules: all, some or none selected. */
function selectionState(list: Schedule[], selected: Set<string>): boolean | "indeterminate" {
  const n = list.filter((s) => selected.has(s.id)).length;
  if (n === 0) return false;
  return n === list.length ? true : "indeterminate";
}

/** MoveMenuItems lists where schedules can be moved: each group, out of any, a new group. */
function MoveMenuItems({
  groups,
  current,
  onMove,
  onNewGroup,
}: {
  groups: ScheduleGroup[];
  /** current is the group every schedule is already in, if they share one. */
  current?: string;
  onMove: (groupId: string, name?: string) => void;
  onNewGroup: () => void;
}) {
  return (
    <>
      {groups.map((g) => (
        <DropdownMenuItem
          key={g.id}
          disabled={current === g.id}
          onSelect={() => onMove(g.id, g.name)}
        >
          {g.name}
        </DropdownMenuItem>
      ))}
      {groups.length > 0 && <DropdownMenuSeparator />}
      <DropdownMenuItem disabled={current === ""} onSelect={() => onMove("")}>
        No group
      </DropdownMenuItem>
      <DropdownMenuItem onSelect={onNewGroup}>
        <FolderPlus aria-hidden />
        New group…
      </DropdownMenuItem>
    </>
  );
}

/** sharedGroup is the group every schedule is in ("" for none), or undefined when they differ. */
function sharedGroup(list: Schedule[]): string | undefined {
  const first = list[0]?.groupId;
  return list.every((s) => s.groupId === first) ? first : undefined;
}

/** TableCtx is what the table's cells share with the page; the columns themselves never change, so cells stay mounted. */
interface TableCtx {
  /** shown is what the list shows: the schedules that pass the filters, in open sections. */
  shown: Schedule[];
  selectedIds: Set<string>;
  select: (list: Schedule[], on: boolean) => void;
  sort: ScheduleSort | undefined;
  onSort: (s: ScheduleSort) => void;
  workers: Map<string, Worker>;
  groups: ScheduleGroup[];
  runNow: (s: Schedule) => Promise<void>;
  setEnabled: (s: Schedule, on: boolean) => Promise<void>;
  moveMany: (list: Schedule[], groupId: string, name?: string) => Promise<void>;
  newGroupFor: (list: Schedule[]) => void;
}

const TableContext = createContext<TableCtx | null>(null);

function useTable(): TableCtx {
  const ctx = useContext(TableContext);
  if (!ctx) throw new Error("schedule table cells need the table context");
  return ctx;
}

function SelectAll() {
  const { shown, selectedIds, select } = useTable();
  return (
    <Checkbox
      checked={selectionState(shown, selectedIds)}
      disabled={!shown.length}
      onCheckedChange={(v) => select(shown, v === true)}
      aria-label="Select every schedule listed"
    />
  );
}

function SelectRow({ schedule: s }: { schedule: Schedule }) {
  const { selectedIds, select } = useTable();
  return (
    <Stop>
      <Checkbox
        checked={selectedIds.has(s.id)}
        onCheckedChange={(v) => select([s], v === true)}
        aria-label={`Select ${s.name}`}
      />
    </Stop>
  );
}

function SortBy({ label, sortKey }: { label: string; sortKey: "name" | "next" | "last" }) {
  const { sort, onSort } = useTable();
  return <SortHeader label={label} sortKey={sortKey} sort={sort} onSort={onSort} />;
}

function TargetCell({ schedule }: { schedule: Schedule }) {
  return <TargetChip schedule={schedule} workers={useTable().workers} />;
}

function EnabledCell({ schedule: s }: { schedule: Schedule }) {
  const { setEnabled } = useTable();
  return (
    <Stop>
      <Switch
        checked={s.enabled}
        onCheckedChange={(on) => void setEnabled(s, on)}
        aria-label={s.enabled ? `Pause ${s.name}` : `Resume ${s.name}`}
      />
    </Stop>
  );
}

function ActionsCell({ schedule: s }: { schedule: Schedule }) {
  const { runNow, setEnabled, groups, moveMany, newGroupFor } = useTable();
  return (
    <Stop>
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${s.name}`}>
            <MoreHorizontal aria-hidden />
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end">
          <DropdownMenuItem onSelect={() => void runNow(s)}>
            <Play aria-hidden />
            Run now
          </DropdownMenuItem>
          <DropdownMenuItem onSelect={() => void setEnabled(s, !s.enabled)}>
            {s.enabled ? <Pause aria-hidden /> : <Play aria-hidden />}
            {s.enabled ? "Pause" : "Resume"}
          </DropdownMenuItem>
          <DropdownMenuSeparator />
          <DropdownMenuItem asChild>
            <Link to="/schedules/$scheduleId/edit" params={{ scheduleId: s.id }}>
              <Pencil aria-hidden />
              Edit
            </Link>
          </DropdownMenuItem>
          <DropdownMenuSub>
            <DropdownMenuSubTrigger>
              <FolderInput aria-hidden />
              Move to group
            </DropdownMenuSubTrigger>
            <DropdownMenuSubContent>
              <MoveMenuItems
                groups={groups}
                current={s.groupId}
                onMove={(id, name) => void moveMany([s], id, name)}
                onNewGroup={() => newGroupFor([s])}
              />
            </DropdownMenuSubContent>
          </DropdownMenuSub>
        </DropdownMenuContent>
      </DropdownMenu>
    </Stop>
  );
}

const columns = [
  col.display({
    id: "select",
    header: () => <SelectAll />,
    cell: ({ row }) => <SelectRow schedule={row.original} />,
    meta: { className: "w-10 pr-0" },
  }),
  col.accessor("name", {
    header: () => <SortBy label="Schedule" sortKey="name" />,
    cell: ({ row: { original: s } }) => (
      <div className="flex w-0 min-w-full flex-col gap-0.5">
        <span className="truncate font-medium" title={s.name}>
          {s.name}
        </span>
        <ScheduleWhat schedule={s} />
      </div>
    ),
    // The column takes the width the others leave; long names are cut.
    meta: { className: "w-full min-w-[16rem]" },
  }),
  col.display({
    id: "when",
    header: "When",
    cell: ({ row: { original: s } }) => (
      <div className="flex flex-col">
        <span className="text-[13px] whitespace-nowrap" title={s.cron}>
          {cronText(s.cron)}
        </span>
        <span className="text-xs whitespace-nowrap text-muted-foreground">{s.timezone}</span>
      </div>
    ),
  }),
  col.display({
    id: "next",
    header: () => <SortBy label="Next run" sortKey="next" />,
    cell: ({ row }) => <NextRun schedule={row.original} />,
  }),
  col.display({
    id: "target",
    header: "Runs on",
    cell: ({ row }) => <TargetCell schedule={row.original} />,
  }),
  col.display({
    id: "last",
    header: () => <SortBy label="Last run" sortKey="last" />,
    cell: ({ row: { original: s } }) =>
      s.lastRun ? (
        <div className="flex items-center gap-2">
          <RunOutcome run={s.lastRun} />
          <RelativeTime
            date={tsDate(s.lastRun.scheduledFor)}
            className="text-xs whitespace-nowrap text-muted-foreground"
          />
        </div>
      ) : (
        <span className="text-[13px] text-muted-foreground">Not yet</span>
      ),
  }),
  col.display({
    id: "enabled",
    header: "On",
    cell: ({ row }) => <EnabledCell schedule={row.original} />,
  }),
  col.display({
    id: "actions",
    header: () => <span className="sr-only">Actions</span>,
    cell: ({ row }) => <ActionsCell schedule={row.original} />,
    meta: { className: "w-12 text-right" },
  }),
];

/** sortColumn is the column a sort key sorts by. */
const sortColumn: Record<string, string> = { name: "name", next: "next", last: "last" };

/** SchedulesPage lists the user's schedules, in sections, with filters and actions on a selection. */
export function SchedulesPage({ search }: { search: SchedulesSearch }) {
  const navigate = useNavigate();
  const setSearch = useCallback(
    (patch: Partial<SchedulesSearch>) =>
      void navigate({ to: "/schedules", search: { ...search, ...patch }, replace: true }),
    [navigate, search],
  );
  const q = useQuery(ScheduleService.method.listSchedules, {}, { refetchInterval: 30_000 });
  const groupsQ = useScheduleGroups();
  const workers = useQuery(WorkerAdminService.method.listWorkers, {});
  const { runNow, setEnabled } = useScheduleActions();
  const bulk = useBulkScheduleActions();
  const workerMap = useMemo(
    () => new Map((workers.data?.workers ?? []).map((w) => [w.id, w])),
    [workers.data],
  );
  const groups = useMemo(() => groupsQ.data?.groups ?? [], [groupsQ.data]);
  const ctx = useMemo(() => ({ workers: workerMap, groups }), [workerMap, groups]);
  const all = q.data?.schedules;
  const by = search.by ?? "group";
  // A group filter on a group that is gone (deleted here or elsewhere) is dropped.
  const staleGroup =
    !!search.group &&
    search.group !== "none" &&
    !!groupsQ.data &&
    !groups.some((g) => g.id === search.group);
  const view = useMemo(
    () => (staleGroup ? { ...search, group: undefined } : search),
    [search, staleGroup],
  );
  const visible = useMemo(
    () =>
      all
        ? sortSchedules(
            all.filter((s) => matches(s, view, ctx)),
            view.sort,
          )
        : undefined,
    [all, view, ctx],
  );
  const isFiltered = filtered(view);
  const secs = useMemo(
    () => (visible ? sections(visible, by, ctx, !isFiltered) : []),
    [visible, by, ctx, isFiltered],
  );
  const total = useMemo(() => counts(all ?? []), [all]);
  const [collapsed, setCollapsed] = useCollapsed(by);
  const isFolded = useCallback(
    (sec: Section) => !!sec.title && collapsed.has(sec.key),
    [collapsed],
  );
  const shown = useMemo(
    () => secs.flatMap((sec) => (isFolded(sec) ? [] : sec.schedules)),
    [secs, isFolded],
  );

  // The selection: only schedules the list shows count. One hidden by a
  // filter or in a folded section is left out (and counts again when shown).
  const [selectedIds, setSelectedIds] = useState<Set<string>>(() => new Set());
  const selected = useMemo(() => shown.filter((s) => selectedIds.has(s.id)), [shown, selectedIds]);
  const select = useCallback((list: Schedule[], on: boolean) => {
    setSelectedIds((prev) => {
      const next = new Set(prev);
      for (const s of list) {
        if (on) next.add(s.id);
        else next.delete(s.id);
      }
      return next;
    });
  }, []);
  const clearSelection = () => setSelectedIds(new Set());

  // Dialogs: a new group (then the schedules move into it), a rename, deletions.
  const [newGroupFor, setNewGroupFor] = useState<Schedule[] | undefined>();
  const [renaming, setRenaming] = useState<ScheduleGroup | undefined>();
  const [deletingGroup, setDeletingGroup] = useState<ScheduleGroup | undefined>();
  const [deleting, setDeleting] = useState<Schedule[] | undefined>();
  const deleteGroup = useDeleteGroup();
  const onSort = useCallback(
    (sort: ScheduleSort) => setSearch({ sort: sort === "name" ? undefined : sort }),
    [setSearch],
  );
  const tableCtx = useMemo<TableCtx>(
    () => ({
      shown,
      selectedIds,
      select,
      sort: search.sort,
      onSort,
      workers: workerMap,
      groups,
      runNow,
      setEnabled,
      moveMany: bulk.moveMany,
      newGroupFor: setNewGroupFor,
    }),
    [
      shown,
      selectedIds,
      select,
      search.sort,
      onSort,
      workerMap,
      groups,
      runNow,
      setEnabled,
      bulk.moveMany,
    ],
  );

  const sectionHeader = (sec: Section): React.ReactNode => {
    if (!sec.title) return undefined;
    const open = !isFolded(sec);
    const anyOn = sec.schedules.some((s) => s.enabled);
    const anyOff = sec.schedules.some((s) => !s.enabled);
    return (
      <div className="flex items-center gap-2">
        <span className="flex w-8 justify-center">
          {/* A folded section's schedules are not selected; ticking it opens it. */}
          <Checkbox
            checked={open ? selectionState(sec.schedules, selectedIds) : false}
            disabled={!sec.schedules.length}
            onCheckedChange={(v) => {
              if (!open && v === true) setCollapsed(sec.key, false);
              select(sec.schedules, v === true);
            }}
            aria-label={`Select every schedule in ${sec.title}`}
          />
        </span>
        <button
          type="button"
          onClick={() => setCollapsed(sec.key, open)}
          aria-expanded={open}
          className="inline-flex min-w-0 items-center gap-1.5 rounded px-1 py-0.5 text-[13px] font-semibold hover:bg-accent"
        >
          <ChevronRight
            className={cn(
              "size-3.5 shrink-0 text-muted-foreground transition-transform",
              open && "rotate-90",
            )}
            aria-hidden
          />
          {sec.key === "attention" && (
            <AlertTriangle className="size-3.5 shrink-0 text-warning" aria-hidden />
          )}
          <span className="truncate">{sec.title}</span>
        </button>
        <span className="tabular rounded-full bg-secondary px-1.5 text-[11px] font-medium text-secondary-foreground">
          {sec.schedules.length}
        </span>
        <span className="ml-auto">
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${sec.title}`}>
                <MoreHorizontal aria-hidden />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              <DropdownMenuLabel className="text-xs font-normal text-muted-foreground">
                {schedulesText(sec.schedules.length)}
              </DropdownMenuLabel>
              <DropdownMenuItem
                disabled={!sec.schedules.length}
                onSelect={() => void bulk.runMany(sec.schedules)}
              >
                <Play aria-hidden />
                Run all now
              </DropdownMenuItem>
              <DropdownMenuItem
                disabled={!anyOn}
                onSelect={() => void bulk.setEnabledMany(sec.schedules, false)}
              >
                <Pause aria-hidden />
                Pause all
              </DropdownMenuItem>
              <DropdownMenuItem
                disabled={!anyOff}
                onSelect={() => void bulk.setEnabledMany(sec.schedules, true)}
              >
                <Play aria-hidden />
                Resume all
              </DropdownMenuItem>
              {sec.group && (
                <>
                  <DropdownMenuSeparator />
                  <DropdownMenuItem asChild>
                    <Link to="/schedules/new" search={{ group: sec.group.id }}>
                      <Plus aria-hidden />
                      New schedule in this group
                    </Link>
                  </DropdownMenuItem>
                  <DropdownMenuItem onSelect={() => setRenaming(sec.group)}>
                    <Pencil aria-hidden />
                    Rename group…
                  </DropdownMenuItem>
                  <DropdownMenuItem
                    className="text-danger focus:text-danger"
                    onSelect={() => setDeletingGroup(sec.group)}
                  >
                    <Trash2 aria-hidden />
                    Delete group…
                  </DropdownMenuItem>
                </>
              )}
            </DropdownMenuContent>
          </DropdownMenu>
        </span>
      </div>
    );
  };

  const tableSections: DataTableSection<Schedule>[] | undefined =
    !all || all.length === 0
      ? undefined
      : secs.map((sec) => ({
          key: sec.key || "none",
          header: sectionHeader(sec),
          rows: sec.schedules,
          collapsed: isFolded(sec),
          empty: sec.group ? (
            <p className="px-4 py-3 text-[13px] text-muted-foreground">
              No schedules in this group yet. Use <strong>Move to group</strong> on schedules, or
              create one here from the group's menu.
            </p>
          ) : undefined,
        }));

  const targets = useMemo(() => {
    const keys = [...new Set((all ?? []).map(targetKey))];
    return keys
      .map((k) => ({ key: k, title: targetTitle(k, workerMap) }))
      .sort((a, b) => a.title.localeCompare(b.title));
  }, [all, workerMap]);
  const kinds = useMemo(() => [...new Set((all ?? []).map(kindOf))].filter(Boolean), [all]);
  const statusChips: { value: StatusFilter | undefined; label: string; n: number }[] = [
    { value: undefined, label: "All", n: total.total },
    { value: "attention", label: statusTitles.attention, n: total.attention },
    { value: "on", label: statusTitles.on, n: total.on },
    { value: "paused", label: statusTitles.paused, n: total.paused },
  ];
  const selectedShared = sharedGroup(selected);

  return (
    <>
      <PageHeader
        title="Schedules"
        description="Jobs that run by themselves, at the times you set, on a worker or on any worker with a label."
        actions={
          <>
            <HelpLink page="schedules" />
            <Button variant="outline" onClick={() => setNewGroupFor([])}>
              <FolderPlus aria-hidden />
              New group
            </Button>
            <Button asChild>
              <Link to="/schedules/new">
                <Plus aria-hidden />
                New schedule
              </Link>
            </Button>
          </>
        }
      />
      {all && all.length > 0 && (
        <>
          <div role="group" aria-label="Status" className="mb-3 flex flex-wrap gap-1.5">
            {statusChips.map((c) => {
              const active = search.status === c.value;
              const warn = c.value === "attention" && c.n > 0;
              return (
                <button
                  key={c.label}
                  type="button"
                  aria-pressed={active}
                  onClick={() => setSearch({ status: c.value })}
                  className={cn(
                    "inline-flex items-center gap-1.5 rounded-full border px-3 py-1 text-[13px] transition-colors",
                    active
                      ? "border-primary bg-primary text-primary-foreground"
                      : "bg-card hover:bg-accent",
                    warn && !active && "border-warning/40 text-warning",
                  )}
                >
                  {c.value === "attention" && <AlertTriangle className="size-3.5" aria-hidden />}
                  {c.label}
                  <span
                    className={cn(
                      "tabular font-semibold",
                      !active && !warn && "text-muted-foreground",
                    )}
                  >
                    {c.n}
                  </span>
                </button>
              );
            })}
          </div>
          <FilterBar
            active={isFiltered}
            onClear={() =>
              void navigate({
                to: "/schedules",
                search: { by: search.by, sort: search.sort },
                replace: true,
              })
            }
          >
            <SearchInput
              value={search.q ?? ""}
              onChange={(v) => setSearch({ q: v || undefined })}
              placeholder="Search names, paths and remotes"
            />
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
              value={search.target ?? ANY}
              onValueChange={(v) => setSearch({ target: v === ANY ? undefined : v })}
            >
              <SelectTrigger className="w-[calc(50%-0.25rem)] bg-card sm:w-44" aria-label="Runs on">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value={ANY}>Any worker</SelectItem>
                <SelectSeparator />
                {targets.map((t) => (
                  <SelectItem key={t.key} value={t.key}>
                    {t.title}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            {groups.length > 0 && (
              <Select
                value={view.group ?? ANY}
                onValueChange={(v) => setSearch({ group: v === ANY ? undefined : v })}
              >
                <SelectTrigger className="w-[calc(50%-0.25rem)] bg-card sm:w-40" aria-label="Group">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value={ANY}>Any group</SelectItem>
                  <SelectSeparator />
                  {groups.map((g) => (
                    <SelectItem key={g.id} value={g.id}>
                      {g.name}
                    </SelectItem>
                  ))}
                  <SelectItem value="none">No group</SelectItem>
                </SelectContent>
              </Select>
            )}
            <div className="flex items-center gap-2 sm:ml-auto">
              <span className="text-[13px] text-muted-foreground">Group by</span>
              <Select
                value={by}
                onValueChange={(v) => setSearch({ by: v === "group" ? undefined : (v as GroupBy) })}
              >
                <SelectTrigger className="w-36 bg-card" aria-label="Group by">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="group">Group</SelectItem>
                  <SelectItem value="target">Worker</SelectItem>
                  <SelectItem value="kind">Kind</SelectItem>
                  <SelectItem value="status">Status</SelectItem>
                  <SelectItem value="none">Nothing</SelectItem>
                </SelectContent>
              </Select>
            </div>
          </FilterBar>
        </>
      )}
      {selected.length > 0 && (
        <div
          role="region"
          aria-label="Selected schedules"
          className="sticky top-2 z-10 mb-3 flex flex-wrap items-center gap-2 rounded-lg border bg-card px-3 py-2 shadow-sm"
        >
          <span className="mr-1 text-[13px] font-medium">{selected.length} selected</span>
          <Button size="sm" variant="outline" onClick={() => void bulk.runMany(selected)}>
            <Play aria-hidden />
            Run now
          </Button>
          <Button
            size="sm"
            variant="outline"
            disabled={!selected.some((s) => s.enabled)}
            onClick={() => void bulk.setEnabledMany(selected, false)}
          >
            <Pause aria-hidden />
            Pause
          </Button>
          <Button
            size="sm"
            variant="outline"
            disabled={!selected.some((s) => !s.enabled)}
            onClick={() => void bulk.setEnabledMany(selected, true)}
          >
            <Play aria-hidden />
            Resume
          </Button>
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button size="sm" variant="outline">
                <FolderInput aria-hidden />
                Move to group
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="start">
              <MoveMenuItems
                groups={groups}
                current={selectedShared}
                onMove={(id, name) => void bulk.moveMany(selected, id, name)}
                onNewGroup={() => setNewGroupFor(selected)}
              />
            </DropdownMenuContent>
          </DropdownMenu>
          <Button
            size="sm"
            variant="outline"
            className="text-danger hover:text-danger"
            onClick={() => setDeleting(selected)}
          >
            <Trash2 aria-hidden />
            Delete
          </Button>
          <Button size="sm" variant="ghost" className="ml-auto" onClick={clearSelection}>
            <X aria-hidden />
            Clear selection
          </Button>
        </div>
      )}
      <TableContext.Provider value={tableCtx}>
        <DataTable
          aria-label="Schedules"
          columns={columns}
          ariaSort={(id) => ariaSort(search.sort, id)}
          data={all}
          sections={tableSections}
          isLoading={q.isPending}
          error={q.error ?? undefined}
          onRetry={() => void q.refetch()}
          getRowId={(s) => s.id}
          rowLabel={(s) => s.name}
          isRowSelected={(s) => selectedIds.has(s.id)}
          onRowClick={(s) =>
            void navigate({ to: "/schedules/$scheduleId", params: { scheduleId: s.id } })
          }
          empty={
            all && all.length > 0 ? (
              <div className="flex flex-col items-center gap-3 p-8 text-center">
                <p className="text-muted-foreground">No schedules match these filters.</p>
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() =>
                    void navigate({
                      to: "/schedules",
                      search: { by: search.by, sort: search.sort },
                      replace: true,
                    })
                  }
                >
                  Clear filters
                </Button>
              </div>
            ) : (
              <EmptyState
                icon={CalendarClock}
                title="No schedules yet"
                description="A schedule runs a job by itself: every night, every hour, or whenever you like. If its worker is asleep, it can run when it wakes up."
                action={
                  <Button asChild>
                    <Link to="/schedules/new">New schedule</Link>
                  </Button>
                }
              />
            )
          }
        />
      </TableContext.Provider>

      {newGroupFor && (
        <GroupNameDialog
          open
          onOpenChange={(o) => !o && setNewGroupFor(undefined)}
          description={
            newGroupFor.length
              ? `The ${schedulesText(newGroupFor.length)} you chose move into it. Groups change nothing about when or where schedules run.`
              : undefined
          }
          onSaved={async (g) => {
            if (newGroupFor.length) await bulk.moveMany(newGroupFor, g.id, g.name);
          }}
        />
      )}
      {renaming && (
        <GroupNameDialog
          key={renaming.id}
          open
          group={renaming}
          onOpenChange={(o) => !o && setRenaming(undefined)}
        />
      )}
      <ConfirmDialog
        open={!!deletingGroup}
        onOpenChange={(o) => !o && setDeletingGroup(undefined)}
        destructive
        title={`Delete the group ${deletingGroup?.name ?? ""}?`}
        description={groupDeleteText(
          (all ?? []).filter((s) => s.groupId === deletingGroup?.id).length,
        )}
        confirmLabel="Delete group"
        onConfirm={async () => {
          if (!deletingGroup) return;
          await deleteGroup(deletingGroup);
          if (search.group === deletingGroup.id) setSearch({ group: undefined });
        }}
      />
      <ConfirmDialog
        open={!!deleting}
        onOpenChange={(o) => !o && setDeleting(undefined)}
        destructive
        title={
          deleting?.length === 1
            ? `Delete ${deleting[0].name}?`
            : `Delete ${schedulesText(deleting?.length ?? 0)}?`
        }
        description="Their run history is deleted with them. Jobs they already ran stay in the job list."
        confirmLabel={deleting?.length === 1 ? "Delete schedule" : "Delete schedules"}
        onConfirm={async () => {
          if (!deleting) return;
          await bulk.deleteMany(deleting);
          select(deleting, false);
        }}
      >
        {deleting && deleting.length > 1 && (
          <ul className="max-h-40 overflow-y-auto rounded-md border px-3 py-2 text-[13px]">
            {deleting.map((s) => (
              <li key={s.id} className="truncate">
                {s.name}
              </li>
            ))}
          </ul>
        )}
      </ConfirmDialog>
    </>
  );
}

/** groupDeleteText says what deleting a group with n schedules does to them. */
function groupDeleteText(n: number): string {
  if (n === 0) return "It has no schedules.";
  if (n === 1) return "Its schedule stays, in no group, and runs as before.";
  return `Its ${n} schedules stay, in no group, and run as before.`;
}

/** useDeleteGroup deletes a group (its schedules stay, in no group) and refreshes the lists. */
function useDeleteGroup() {
  const queryClient = useQueryClient();
  const del = useMutation(ScheduleService.method.deleteScheduleGroup).mutateAsync;
  return useCallback(
    async (g: ScheduleGroup) => {
      await del({ groupId: g.id });
      await invalidateService(queryClient, ScheduleService);
      toast.success(`Deleted the group ${g.name}`);
    },
    [del, queryClient],
  );
}
