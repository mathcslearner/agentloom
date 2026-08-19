import { problem, type DefinitionView } from "@agentloom/api-client";
import { serverApi } from "@/lib/api/server";
import { isConfigured } from "@/lib/config";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";

export const dynamic = "force-dynamic";

/** Server component: renders the latest-per-name definition list via the
 *  server-held key. Exercises the direct (non-proxy) client path. */
export default async function DefinitionsPage() {
  if (!isConfigured()) {
    return <SetupNotice />;
  }

  const { data, error } = await serverApi().GET("/v1/definitions", {
    params: { query: { limit: 50 } },
  });

  if (error) {
    return <ErrorNotice message={problem(error)?.message ?? "failed to load definitions"} />;
  }

  const definitions = data.definitions;
  return (
    <div className="space-y-4">
      <h1 className="text-xl font-semibold tracking-tight">Definitions</h1>
      {definitions.length === 0 ? (
        <p className="text-sm text-muted-foreground">No definitions registered yet.</p>
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Version</TableHead>
              <TableHead>ID</TableHead>
              <TableHead>Created</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {definitions.map((d: DefinitionView) => (
              <TableRow key={d.id}>
                <TableCell className="font-medium">{d.name}</TableCell>
                <TableCell>v{d.version}</TableCell>
                <TableCell className="font-mono text-xs text-muted-foreground">{d.id}</TableCell>
                <TableCell className="text-muted-foreground">
                  {new Date(d.created_at).toLocaleString()}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </div>
  );
}

function SetupNotice() {
  return (
    <Card>
      <CardHeader>
        <CardTitle>Backend not configured</CardTitle>
      </CardHeader>
      <CardContent className="text-sm text-muted-foreground">
        Set <code className="rounded bg-muted px-1">AGENTLOOM_API_KEY</code> in{" "}
        <code className="rounded bg-muted px-1">web/app/.env.local</code>.
      </CardContent>
    </Card>
  );
}

function ErrorNotice({ message }: { message: string }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>Could not load definitions</CardTitle>
      </CardHeader>
      <CardContent className="text-sm text-muted-foreground">{message}</CardContent>
    </Card>
  );
}
