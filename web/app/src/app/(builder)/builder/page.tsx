import { BuilderShell } from "@/components/builder/BuilderShell";

// The visual DAG builder (M17). A thin server component hosting the client
// BuilderShell, which owns the React Flow canvas, palette, and inspector. The
// builder starts from an empty document; `?definition=<id>` opens a stored
// definition (17.6 "open in builder"), loaded client-side through the proxy.
export const metadata = { title: "Builder — agentloom" };

export default async function BuilderPage({
  searchParams,
}: {
  searchParams: Promise<Record<string, string | string[] | undefined>>;
}) {
  const sp = await searchParams;
  const raw = sp["definition"];
  const openDefinitionId = typeof raw === "string" ? raw : undefined;
  return <BuilderShell openDefinitionId={openDefinitionId} />;
}
