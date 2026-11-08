import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { JobStatus, WorkerStatus } from "@/gen/syncr/v1/api_pb";

import { StatusBadge } from "../status-badge";

describe("StatusBadge", () => {
  it.each([
    [JobStatus.QUEUED, "Queued", "neutral"],
    [JobStatus.DISPATCHED, "Starting", "live"],
    [JobStatus.RUNNING, "Running", "live"],
    [JobStatus.SUCCEEDED, "Succeeded", "success"],
    [JobStatus.FAILED, "Failed", "danger"],
    [JobStatus.CANCELLED, "Cancelled", "neutral"],
    [JobStatus.INTERRUPTED, "Interrupted", "warning"],
  ])("labels job status %s as %s", (status, label, tone) => {
    render(<StatusBadge job={status} />);
    expect(screen.getByText(label)).toHaveAttribute("data-tone", tone);
  });

  it("shows a requested cancel only while the job is active", () => {
    const { rerender } = render(<StatusBadge job={JobStatus.RUNNING} cancelRequested />);
    expect(screen.getByText("Cancelling")).toHaveAttribute("data-tone", "warning");
    rerender(<StatusBadge job={JobStatus.SUCCEEDED} cancelRequested />);
    expect(screen.getByText("Succeeded")).toBeInTheDocument();
  });

  it.each([
    [WorkerStatus.ONLINE, "Online", "success"],
    [WorkerStatus.OFFLINE, "Offline", "neutral"],
    [WorkerStatus.PENDING, "Pending", "warning"],
    [WorkerStatus.UNSPECIFIED, "Unknown", "neutral"],
  ])("labels worker status %s as %s", (status, label, tone) => {
    render(<StatusBadge worker={status} />);
    expect(screen.getByText(label)).toHaveAttribute("data-tone", tone);
  });
});
