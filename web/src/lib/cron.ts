import cronstrue from "cronstrue";

/** CronPreset is a ready-made expression offered by CronInput. */
export interface CronPreset {
  label: string;
  expr: string;
}

/** cronPresets are the common schedules, from most to least frequent. */
export const cronPresets: CronPreset[] = [
  { label: "Every 15 minutes", expr: "*/15 * * * *" },
  { label: "Every hour", expr: "0 * * * *" },
  { label: "Every day at 02:30", expr: "30 2 * * *" },
  { label: "Weekdays at 18:00", expr: "0 18 * * 1-5" },
  { label: "Sundays at 03:00", expr: "0 3 * * 0" },
  { label: "The 1st of each month at 04:00", expr: "0 4 1 * *" },
];

const descriptors: Record<string, string> = {
  "@yearly": "0 0 1 1 *",
  "@annually": "0 0 1 1 *",
  "@monthly": "0 0 1 * *",
  "@weekly": "0 0 * * 0",
  "@daily": "0 0 * * *",
  "@midnight": "0 0 * * *",
  "@hourly": "0 * * * *",
};

/** CronDescription is a readable cron expression, or why it can't be read. */
export type CronDescription = { ok: true; text: string } | { ok: false; error: string };

/**
 * describeCron renders a five-field expression (or a descriptor such as
 * @daily) in words. It checks the shape only; the controller has the final
 * say on whether an expression is valid.
 */
export function describeCron(expr: string): CronDescription {
  const e = expr.trim().replace(/\s+/g, " ");
  if (!e) return { ok: false, error: "Enter a schedule, or pick one of the presets." };
  if (e.startsWith("@every"))
    return { ok: false, error: "Intervals like @every are not supported. Use five fields." };
  if (/^(CRON_)?TZ=/i.test(e))
    return { ok: false, error: "Set the time zone below instead of in the expression." };
  let five = e;
  if (e.startsWith("@")) {
    const d = descriptors[e.toLowerCase()];
    if (!d) return { ok: false, error: `${e} is not a known shortcut.` };
    five = d;
  }
  const n = five.split(" ").length;
  if (n !== 5) {
    return {
      ok: false,
      error:
        n === 6
          ? "Seconds are not supported. Use five fields: minute, hour, day of month, month and day of week."
          : "Use five fields: minute, hour, day of month, month and day of week.",
    };
  }
  try {
    const text = cronstrue.toString(five, {
      use24HourTimeFormat: true,
      throwExceptionOnParseError: true,
      verbose: false,
    });
    return { ok: true, text };
  } catch (err) {
    const msg = typeof err === "string" ? err : err instanceof Error ? err.message : "";
    return {
      ok: false,
      error: msg ? msg.replace(/^Error: /, "") : "This is not a valid schedule.",
    };
  }
}

/** cronText is describeCron's text, or the raw expression when it can't be read. */
export function cronText(expr: string): string {
  const d = describeCron(expr);
  return d.ok ? d.text : expr;
}
