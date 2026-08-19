import { BuilderShell } from "@/components/builder/BuilderShell";

// The visual DAG builder (M17). A thin server component hosting the client
// BuilderShell, which owns the React Flow canvas, palette, and inspector. The
// builder starts from an empty document; opening a stored definition (17.6)
// calls the store's `load`.
export const metadata = { title: "Builder — agentloom" };

export default function BuilderPage() {
  return <BuilderShell />;
}
