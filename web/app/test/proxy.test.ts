import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// The route reads process.env via @/lib/config; set it before importing.
const ORIGINAL_ENV = { ...process.env };

async function importRoute() {
  vi.resetModules();
  return import("@/app/api/agentloom/[...path]/route");
}

function ctx(path: string[]) {
  return { params: Promise.resolve({ path }) };
}

describe("same-origin API proxy", () => {
  beforeEach(() => {
    process.env.AGENTLOOM_API_URL = "http://backend.test:8080";
    process.env.AGENTLOOM_API_KEY = "sk_server_secret";
  });
  afterEach(() => {
    process.env = { ...ORIGINAL_ENV };
    vi.restoreAllMocks();
  });

  it("injects the bearer key and forwards method, path, and query", async () => {
    const seen: { url: string; headers: Headers; method: string } = {
      url: "",
      headers: new Headers(),
      method: "",
    };
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        seen.url = String(input);
        seen.headers = new Headers(init?.headers);
        seen.method = init?.method ?? "GET";
        return new Response(JSON.stringify({ runs: [] }), {
          status: 200,
          headers: { "content-type": "application/json", "x-ratelimit-remaining": "42" },
        });
      }),
    );

    const { GET } = await importRoute();
    const req = new Request("http://localhost:3000/api/agentloom/v1/runs?status=running&limit=5");
    const res = await GET(req, ctx(["v1", "runs"]));

    expect(res.status).toBe(200);
    expect(seen.method).toBe("GET");
    expect(seen.url).toBe("http://backend.test:8080/v1/runs?status=running&limit=5");
    expect(seen.headers.get("authorization")).toBe("Bearer sk_server_secret");
    // Rate-limit header is surfaced to the browser.
    expect(res.headers.get("x-ratelimit-remaining")).toBe("42");
  });

  it("forwards a POST body and the idempotency-key header", async () => {
    let bodyText = "";
    let idem: string | null = null;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
        bodyText = new TextDecoder().decode(init?.body as ArrayBuffer);
        idem = new Headers(init?.headers).get("idempotency-key");
        return new Response(JSON.stringify({ run: { id: "r1" } }), { status: 200 });
      }),
    );

    const { POST } = await importRoute();
    const req = new Request("http://localhost:3000/api/agentloom/v1/runs", {
      method: "POST",
      headers: { "content-type": "application/json", "idempotency-key": "tok-1" },
      body: JSON.stringify({ definition: { name: "x" } }),
    });
    const res = await POST(req, ctx(["v1", "runs"]));

    expect(res.status).toBe(200);
    expect(JSON.parse(bodyText)).toEqual({ definition: { name: "x" } });
    expect(idem).toBe("tok-1");
  });

  it("never forwards a client-supplied Authorization header (the proxy sets its own)", async () => {
    let forwarded: string | null = null;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
        forwarded = new Headers(init?.headers).get("authorization");
        return new Response("{}", { status: 200 });
      }),
    );

    const { GET } = await importRoute();
    const req = new Request("http://localhost:3000/api/agentloom/v1/runs", {
      headers: { authorization: "Bearer sk_attacker_override" },
    });
    await GET(req, ctx(["v1", "runs"]));
    expect(forwarded).toBe("Bearer sk_server_secret");
  });

  it("rejects paths outside the v1/healthz allowlist with 404 (no open relay)", async () => {
    const fetchSpy = vi.fn();
    vi.stubGlobal("fetch", fetchSpy);
    const { GET } = await importRoute();
    const req = new Request("http://localhost:3000/api/agentloom/internal/keys");
    const res = await GET(req, ctx(["internal", "keys"]));
    expect(res.status).toBe(404);
    expect(fetchSpy).not.toHaveBeenCalled();
  });

  it("returns 503 with an envelope when no server key is configured", async () => {
    delete process.env.AGENTLOOM_API_KEY;
    const fetchSpy = vi.fn();
    vi.stubGlobal("fetch", fetchSpy);
    const { GET } = await importRoute();
    const req = new Request("http://localhost:3000/api/agentloom/v1/runs");
    const res = await GET(req, ctx(["v1", "runs"]));
    expect(res.status).toBe(503);
    const body = (await res.json()) as { error: { code: string } };
    expect(body.error.code).toBe("unauthorized");
    expect(fetchSpy).not.toHaveBeenCalled();
  });

  it("preserves a literal ':decide' verb suffix (ticket 18.5)", async () => {
    // encodeURIComponent would turn ':' into '%3A', which chi (matching on the
    // raw path with ':' as its param delimiter) would 404. The proxy must keep
    // it literal so approvals/{id}:decide reaches the handler.
    let url = "";
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        url = String(input);
        return new Response("{}", { status: 200, headers: { "content-type": "application/json" } });
      }),
    );
    const { POST } = await importRoute();
    const id = "11111111-2222-3333-4444-555555555555";
    const req = new Request(`http://localhost:3000/api/agentloom/v1/approvals/${id}:decide`, {
      method: "POST",
      body: JSON.stringify({ decision: "approve" }),
    });
    await POST(req, ctx(["v1", "approvals", `${id}:decide`]));
    expect(url).toBe(`http://backend.test:8080/v1/approvals/${id}:decide`);
    expect(url).not.toContain("%3A");
  });
});
