import { useNavigate } from "@tanstack/react-router";
import {
  ArrowLeftRight,
  CalendarClock,
  ChevronDown,
  Copy,
  DatabaseBackup,
  PackagePlus,
  Plus,
} from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { pathCrumbs } from "@/lib/paths";

import {
  backupDraft,
  bisyncDraft,
  draftFolder,
  remoteBackupDraft,
  transferDraft,
  type DraftTarget,
  type JobDraft,
} from "./job-drafts";
import { itemsText } from "./transfer";

/**
 * JobDraftMenu starts a job from the file browser in the existing editors,
 * filled in: a backup of the selection (a worker's folders) or of a folder
 * of a remote, a copy or sync, a two-way sync of a folder, or a schedule of
 * one of these; and, in a remote, a new restic repository in the folder.
 * It acts on the selection, or on the open folder when nothing is selected.
 */
export function JobDraftMenu({ target }: { target: DraftTarget }) {
  const navigate = useNavigate();
  const folder = draftFolder(target);
  const kind = target.place.kind;
  const name = (p: string) => (p === "" ? "the root" : (pathCrumbs(p, kind).at(-1)?.label ?? p));
  const what =
    target.items.length > 0
      ? `For the ${itemsText(target.items.length)} selected`
      : folder !== undefined
        ? `For ${name(folder)}`
        : "Open a folder or select items first";
  const backup = kind === "worker" ? backupDraft(target) : remoteBackupDraft(target);
  // A Local directory remote is a folder on a worker: it is backed up from there.
  const localRemote = target.place.kind === "remote" && !!target.place.local;
  const transfer = transferDraft(target);
  const bisync = bisyncDraft(target);
  const go = (to: "/jobs/new" | "/schedules/new", jobDraft: JobDraft) =>
    void navigate({ to, state: (prev) => ({ ...prev, jobDraft }) });
  const jobs: { label: string; icon: typeof Copy; draft: JobDraft | undefined; hide?: boolean }[] =
    [
      {
        label: kind === "worker" ? "Back up with restic" : "Back up this folder",
        icon: DatabaseBackup,
        draft: backup,
        hide: localRemote,
      },
      { label: "Copy or sync", icon: Copy, draft: transfer },
      { label: "Two-way sync", icon: ArrowLeftRight, draft: bisync },
    ];
  const shown = jobs.filter((j) => !j.hide);
  const repoFolder = kind === "remote" ? folder : undefined;

  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant="outline" size="sm">
          <Plus aria-hidden />
          New job
          <ChevronDown aria-hidden />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="start" className="w-64">
        <DropdownMenuLabel className="truncate font-normal text-muted-foreground">
          {what}
        </DropdownMenuLabel>
        <DropdownMenuGroup>
          {shown.map((j) => (
            <DropdownMenuItem
              key={j.label}
              disabled={!j.draft}
              onSelect={() => j.draft && go("/jobs/new", j.draft)}
            >
              <j.icon aria-hidden />
              {j.label}…
            </DropdownMenuItem>
          ))}
        </DropdownMenuGroup>
        <DropdownMenuSeparator />
        <DropdownMenuGroup>
          {shown.map((j) => (
            <DropdownMenuItem
              key={j.label}
              disabled={!j.draft}
              onSelect={() => j.draft && go("/schedules/new", j.draft)}
            >
              <CalendarClock aria-hidden />
              Schedule: {j.label.toLowerCase()}…
            </DropdownMenuItem>
          ))}
        </DropdownMenuGroup>
        {kind === "remote" && target.place.kind === "remote" && (
          <>
            <DropdownMenuSeparator />
            <DropdownMenuItem
              disabled={repoFolder === undefined}
              onSelect={() =>
                target.place.kind === "remote" &&
                void navigate({
                  to: "/repositories/new",
                  search: { remote: target.place.remoteId, path: repoFolder || undefined },
                })
              }
            >
              <PackagePlus aria-hidden />
              New restic repository here
            </DropdownMenuItem>
          </>
        )}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
