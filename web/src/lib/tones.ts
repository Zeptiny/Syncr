import type { Tone } from "./status";

/** toneClasses colours a soft pill by tone. */
export const toneClasses: Record<Tone, string> = {
  live: "bg-live-soft text-live",
  success: "bg-success-soft text-success",
  danger: "bg-danger-soft text-danger",
  warning: "bg-warning-soft text-warning",
  neutral: "bg-neutral-soft text-neutral",
};

/** toneDot colours a status dot by tone. */
export const toneDot: Record<Tone, string> = {
  live: "bg-live",
  success: "bg-success",
  danger: "bg-danger",
  warning: "bg-warning",
  neutral: "bg-neutral/60",
};
