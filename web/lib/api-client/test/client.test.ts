import { describe, expect, it, vi } from "vitest";
import { createApiClient, problem } from "../src/index.js";

/** A fetch double that captures the request and returns a canned JSON body. */
function fakeFetch(status: number, body: unknown) {
  const calls: Request[] = [];
  const fn = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const req = new Request(input as string, init);
    calls.push(req);
    return new Response(JSON.stringify(body), {
      status,
      headers: { "content-type": "application/json" },
    });
  });
  return { fn: fn as unknown as typeof fetch, calls };
}

describe("createApiClient", () => {
  it("attaches a bearer header when an apiKey is given", async () => {
    const { fn, calls } = fakeFetch(200, { definitions: [] });
    const client = createApiClient({ baseUrl: "http://api.test", apiKey: "sk_test", fetch: fn });
    await client.GET("/v1/definitions");
    expect(calls).toHaveLength(1);
    expect(calls[0]!.headers.get("authorization")).toBe("Bearer sk_test");
    expect(calls[0]!.url).toBe("http://api.test/v1/definitions");
  });

  it("sends no Authorization header when no apiKey is given (browser/proxy mode)", async () => {
    const { fn, calls } = fakeFetch(200, { runs: [] });
    const client = createApiClient({ baseUrl: "http://proxy.test", fetch: fn });
    await client.GET("/v1/runs");
    expect(calls[0]!.headers.get("authorization")).toBeNull();
  });

  it("serializes query params through the typed surface", async () => {
    const { fn, calls } = fakeFetch(200, { runs: [] });
    const client = createApiClient({ baseUrl: "http://api.test", apiKey: "sk_x", fetch: fn });
    await client.GET("/v1/runs", { params: { query: { status: "running", limit: 5 } } });
    const url = new URL(calls[0]!.url);
    expect(url.searchParams.get("status")).toBe("running");
    expect(url.searchParams.get("limit")).toBe("5");
  });

  it("returns typed data on success and the error envelope on failure", async () => {
    const ok = fakeFetch(200, { definitions: [{ id: "d1", name: "x", version: 1, created_at: "t" }] });
    const okClient = createApiClient({ baseUrl: "http://api.test", apiKey: "k", fetch: ok.fn });
    const good = await okClient.GET("/v1/definitions");
    expect(good.data?.definitions[0]?.name).toBe("x");
    expect(good.error).toBeUndefined();

    const bad = fakeFetch(404, { error: { code: "run_not_found", message: "no such run" } });
    const badClient = createApiClient({ baseUrl: "http://api.test", apiKey: "k", fetch: bad.fn });
    const res = await badClient.GET("/v1/runs/{run_id}", { params: { path: { run_id: "nope" } } });
    expect(res.data).toBeUndefined();
    expect(problem(res.error)?.code).toBe("run_not_found");
  });
});

describe("problem", () => {
  it("narrows a conforming envelope", () => {
    const detail = problem({ error: { code: "forbidden", message: "missing scope", issues: [] } });
    expect(detail?.code).toBe("forbidden");
    expect(detail?.message).toBe("missing scope");
  });

  it("returns null for non-conforming values", () => {
    expect(problem(null)).toBeNull();
    expect(problem("boom")).toBeNull();
    expect(problem({})).toBeNull();
    expect(problem({ error: "x" })).toBeNull();
    expect(problem({ error: { code: 1, message: "x" } })).toBeNull();
    expect(problem({ error: { code: "x" } })).toBeNull();
  });
});
