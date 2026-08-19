import { RunDetail } from "@/components/dashboard/RunDetail";

// Server component: resolves the route param and hands off to the live client
// view. The run's state and event feed load client-side over the WebSocket.
export default async function RunDetailPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  return <RunDetail runId={id} />;
}
