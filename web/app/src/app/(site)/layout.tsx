// The (site) route group: the list/detail pages (home, definitions, runs) laid
// out in a centred max-width column. A route group leaves the URL unchanged
// (`/`, `/definitions`, `/runs`); it only scopes this layout to those pages so
// the full-bleed builder can use its own.
export default function SiteLayout({ children }: { children: React.ReactNode }) {
  return <main className="mx-auto w-full max-w-6xl px-6 py-8">{children}</main>;
}
