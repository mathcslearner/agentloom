import { describe, expect, it, vi, beforeEach } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import type { Permissions } from "@/lib/pure/dashboard/permissions";
import { controlStateFor, can as canFor } from "@/lib/pure/dashboard/permissions";

// The rendered-permissions DoD (18.6): controls hidden/disabled without the
// required scope. We mock the permissions provider to a fixed state and the
// action hook to a no-op, then assert the run-control buttons' presence/disabled.
let perms: Permissions = { status: "ready", scopes: ["submit"] };
vi.mock("@/lib/permissions", () => ({
  usePermissions: () => ({
    ...perms,
    can: (w: Parameters<typeof canFor>[1]) => canFor(perms, w),
    controlState: (c: Parameters<typeof controlStateFor>[1]) => controlStateFor(perms, c),
  }),
}));
vi.mock("@/lib/dashboard/useRunControls", () => ({
  useRunControls: () => ({
    pending: false,
    cancel: vi.fn().mockResolvedValue(true),
    park: vi.fn().mockResolvedValue(true),
    unpark: vi.fn().mockResolvedValue(true),
    requeue: vi.fn().mockResolvedValue(true),
  }),
}));

import { RunControls } from "@/components/dashboard/RunControls";
import type { RunView } from "@agentloom/api-client";
import type { RunController } from "@/lib/dashboard/run-controller";

const run = (status: RunView["status"]): RunView =>
  ({ id: "r1", status, on_failure: "fail_fast", steps_total: 1, steps_succeeded: 0, steps_failed: 0, steps_skipped: 0, steps_cancelled: 0, steps_collected: 0, created_at: "t", event_seq: 0, cost: {} }) as unknown as RunView;

const controller = {} as RunController;

beforeEach(() => {
  cleanup();
  perms = { status: "ready", scopes: ["submit"] };
});

describe("RunControls rendered permissions", () => {
  it("shows enabled Park + Cancel for a running run with submit", () => {
    render(<RunControls run={run("running")} controller={controller} />);
    expect(screen.getByTestId("run-park")).toBeEnabled();
    expect(screen.getByTestId("run-cancel")).toBeEnabled();
    expect(screen.queryByTestId("run-unpark")).toBeNull();
  });

  it("shows Unpark + Cancel for a parked run", () => {
    render(<RunControls run={run("parked")} controller={controller} />);
    expect(screen.getByTestId("run-unpark")).toBeInTheDocument();
    expect(screen.getByTestId("run-cancel")).toBeInTheDocument();
    expect(screen.queryByTestId("run-park")).toBeNull();
  });

  it("renders nothing for a terminal run", () => {
    render(<RunControls run={run("succeeded")} controller={controller} />);
    expect(screen.queryByTestId("run-controls")).toBeNull();
  });

  it("hides the whole group when the key lacks submit (read-only)", () => {
    perms = { status: "ready", scopes: ["read"] };
    render(<RunControls run={run("running")} controller={controller} />);
    expect(screen.queryByTestId("run-controls")).toBeNull();
  });

  it("disables the controls while permissions load", () => {
    perms = { status: "loading", scopes: [] };
    render(<RunControls run={run("running")} controller={controller} />);
    expect(screen.getByTestId("run-park")).toBeDisabled();
    expect(screen.getByTestId("run-cancel")).toBeDisabled();
  });

  it("enables the controls when whoami is unavailable (server enforces)", () => {
    perms = { status: "unavailable", scopes: [] };
    render(<RunControls run={run("running")} controller={controller} />);
    expect(screen.getByTestId("run-park")).toBeEnabled();
  });
});
