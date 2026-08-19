// The (builder) route group: full-bleed layout for the visual DAG builder. The
// canvas fills the viewport below the header (min-h-0 lets the flex child shrink
// so the React Flow surface can own the remaining height).
export default function BuilderLayout({ children }: { children: React.ReactNode }) {
  return <div className="flex min-h-0 flex-1 flex-col">{children}</div>;
}
