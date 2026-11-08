// URL state of the schedule pages, kept apart from them for code splitting.

/** NewScheduleSearch says what a new schedule starts from. */
export interface NewScheduleSearch {
  /** from is a schedule to duplicate. */
  from?: string;
  /** job is a job to repeat ("Schedule this job"). */
  job?: string;
}

/** validateNewScheduleSearch parses /schedules/new's search params. */
export function validateNewScheduleSearch(s: Record<string, unknown>): NewScheduleSearch {
  const str = (v: unknown) => (typeof v === "string" && v ? v : undefined);
  return { from: str(s.from), job: str(s.job) };
}
