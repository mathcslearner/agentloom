// The (dashboard) route group: the live run-detail view, laid out full-bleed
// (graph pane · inspector · timeline strip) like the builder. A route group
// leaves the URL unchanged (`/runs/{id}`); it only scopes this layout.
export default function DashboardLayout({ children }: { children: React.ReactNode }) {
  return <main className="flex min-h-0 flex-1 flex-col">{children}</main>;
}
