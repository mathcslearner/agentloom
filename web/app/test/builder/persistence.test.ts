import { afterEach, describe, expect, it, vi } from "vitest";
import { createApiClient } from "@agentloom/api-client";

// The persistence functions call browserApi() (the proxy-targeting client). Its
// relative base URL is unresolvable in jsdom, so we mock the module to return a
// real openapi-fetch client with an absolute base and an injectable fetch — the
// persistence logic (create/append/submit, error classification, headers) stays
// under test end-to-end.
interface Handler {
  (req: Request): { status: number; body: unknown };
}
let currentHandler: Handler = () => ({ status: 500, body: {} });
let seenRequests: Request[] = [];

const testFetch = async (input: RequestInfo | URL, init?: RequestInit) => {
  const req = input instanceof Request ? input : new Request(input, init);
  seenRequests.push(req);
  const { status, body } = currentHandler(req);
  return new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
};

vi.mock("@/lib/api/browser", () => ({
  browserApi: () => createApiClient({ baseUrl: "http://test.local", fetch: testFetch }),
}));

import {
  appendDefinitionVersion,
  createDefinition,
  submitRun,
  type DefinitionValue,
} from "@/lib/builder/persistence";

const DEF = { schema_version: 1, name: "wf", steps: [], edges: [] } as unknown as DefinitionValue;

function stubFetch(handler: Handler): Request[] {
  currentHandler = handler;
  seenRequests = [];
  return seenRequests;
}

afterEach(() => {
  seenRequests = [];
});

describe("createDefinition", () => {
  it("returns created on 201", async () => {
    stubFetch(() => ({ status: 201, body: { id: "d1", name: "wf", version: 1, created_at: "", spec: {} } }));
    const res = await createDefinition("wf", DEF);
    expect(res).toEqual({ kind: "created", id: "d1", name: "wf", version: 1 });
  });

  it("maps a 409 name conflict", async () => {
    stubFetch(() => ({ status: 409, body: { error: { code: "conflict", message: "exists" } } }));
    const res = await createDefinition("wf", DEF);
    expect(res.kind).toBe("name_conflict");
  });

  it("maps a 400 invalid_definition with issues", async () => {
    stubFetch(() => ({
      status: 400,
      body: {
        error: { code: "invalid_definition", message: "bad", issues: [{ severity: "error", path: "steps[0].id", msg: "x" }] },
      },
    }));
    const res = await createDefinition("wf", DEF);
    expect(res.kind).toBe("invalid");
    if (res.kind === "invalid") expect(res.issues).toHaveLength(1);
  });
});

describe("appendDefinitionVersion", () => {
  it("sends the If-Match header when expected is given", async () => {
    const seen = stubFetch(() => ({ status: 201, body: { id: "d2", name: "wf", version: 3, created_at: "", spec: {} } }));
    const res = await appendDefinitionVersion("wf", DEF, 2);
    expect(res).toEqual({ kind: "appended", id: "d2", name: "wf", version: 3 });
    expect(seen[0]!.headers.get("if-match")).toBe("2");
  });

  it("omits If-Match when expected is undefined", async () => {
    const seen = stubFetch(() => ({ status: 201, body: { id: "d2", name: "wf", version: 3, created_at: "", spec: {} } }));
    await appendDefinitionVersion("wf", DEF);
    expect(seen[0]!.headers.get("if-match")).toBeNull();
  });

  it("maps a 409 version_conflict (stale save)", async () => {
    stubFetch(() => ({ status: 409, body: { error: { code: "version_conflict", message: "stale" } } }));
    const res = await appendDefinitionVersion("wf", DEF, 1);
    expect(res.kind).toBe("version_conflict");
  });
});

describe("submitRun", () => {
  it("submits by definition_id when given, with an Idempotency-Key", async () => {
    const seen = stubFetch(() => ({ status: 201, body: { run_id: "r1", status: "running", entry_steps: [] } }));
    const res = await submitRun({ definitionId: "d1", params: { topic: "x" }, idempotencyKey: "key-1" });
    expect(res).toEqual({ kind: "submitted", runId: "r1", status: "running", reused: false });
    const body = JSON.parse(await seen[0]!.text());
    expect(body.definition_id).toBe("d1");
    expect(body.params).toEqual({ topic: "x" });
    expect(seen[0]!.headers.get("idempotency-key")).toBe("key-1");
  });

  it("submits inline when no definition_id", async () => {
    const seen = stubFetch(() => ({ status: 200, body: { run_id: "r2", status: "running", entry_steps: [], reused: true } }));
    const res = await submitRun({ def: DEF });
    expect(res).toEqual({ kind: "submitted", runId: "r2", status: "running", reused: true });
    const body = JSON.parse(await seen[0]!.text());
    expect(body.definition).toBeDefined();
    expect(body.definition_id).toBeUndefined();
  });

  it("maps a 400 to invalid with issues", async () => {
    stubFetch(() => ({ status: 400, body: { error: { code: "invalid_definition", message: "bad", issues: [] } } }));
    const res = await submitRun({ def: DEF });
    expect(res.kind).toBe("invalid");
  });
});
