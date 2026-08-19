import Link from "next/link";
import { serverApi } from "@/lib/api/server";
import { isConfigured, serverConfig } from "@/lib/config";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";

export const dynamic = "force-dynamic";

async function backendHealth(): Promise<"ok" | "unreachable"> {
  try {
    const { response } = await serverApi().GET("/healthz");
    return response.ok ? "ok" : "unreachable";
  } catch {
    return "unreachable";
  }
}

export default async function HomePage() {
  const configured = isConfigured();
  const { apiUrl } = serverConfig();
  const health = configured ? await backendHealth() : "unreachable";

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">agentloom</h1>
        <p className="text-muted-foreground">Visual DAG builder and live execution dashboard.</p>
      </div>

      {!configured ? (
        <Card>
          <CardHeader>
            <CardTitle>Set up backend access</CardTitle>
            <CardDescription>
              The web server needs an API key to reach the backend. It is held server-side and
              never shipped to the browser.
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-2 text-sm">
            <p>
              Copy <code className="rounded bg-muted px-1">web/app/.env.example</code> to{" "}
              <code className="rounded bg-muted px-1">web/app/.env.local</code> and set{" "}
              <code className="rounded bg-muted px-1">AGENTLOOM_API_KEY</code> (for the compose
              stack, use <code className="rounded bg-muted px-1">AGENTLOOM_API_ROOT_KEY</code> or a
              key minted with <code className="rounded bg-muted px-1">ctl keys create</code>).
            </p>
          </CardContent>
        </Card>
      ) : (
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2">
              Backend
              <Badge variant={health === "ok" ? "succeeded" : "failed"}>{health}</Badge>
            </CardTitle>
            <CardDescription>{apiUrl}</CardDescription>
          </CardHeader>
          <CardContent className="flex gap-4 text-sm">
            <Link href="/definitions" className="underline hover:no-underline">
              Browse definitions
            </Link>
            <Link href="/runs" className="underline hover:no-underline">
              Browse runs
            </Link>
          </CardContent>
        </Card>
      )}
    </div>
  );
}
