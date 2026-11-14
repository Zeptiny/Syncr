import { describe, expect, it } from "vitest";

import { emptySchedule, labelTargetAllowed, singleWorkerReason, validateSchedule } from "./model";

describe("schedules that must target one worker", () => {
  it("refuses a label for a backup of a remote, with the controller's reason", () => {
    const s = emptySchedule("UTC", "restic_backup", "repo");
    if (s.definition.kind !== "restic_backup") throw new Error("not a backup");
    expect(labelTargetAllowed(s.definition)).toBe(true);
    s.definition.backup = { ...s.definition.backup, source: "remote" };
    expect(singleWorkerReason(s.definition)).toBe(
      "A backup of a remote keeps its mirror or mount folder on one worker",
    );
    const onLabel = {
      ...s,
      name: "n",
      target: { kind: "label" as const, workerId: "", label: "nas" },
    };
    expect(validateSchedule(onLabel).target).toBe(
      "A backup of a remote keeps its mirror or mount folder on one worker: choose a worker.",
    );
  });

  it("keeps the two-way sync rule", () => {
    const s = emptySchedule("UTC", "rclone_bisync");
    expect(singleWorkerReason(s.definition)).toBe("A two-way sync keeps its state on one worker");
    expect(labelTargetAllowed(emptySchedule("UTC", "restic_check").definition)).toBe(true);
  });
});
