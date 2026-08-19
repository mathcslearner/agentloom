import type { Metadata } from "next";
import Link from "next/link";
import "./globals.css";
import { Toaster } from "@/components/ui/toast";
import { RuntimeConfigProvider } from "@/lib/runtime-config";
import { serverConfig } from "@/lib/config";

export const metadata: Metadata = {
  title: "agentloom",
  description: "Visual DAG builder and live execution dashboard for agentloom.",
};

const NAV = [
  { href: "/", label: "Home" },
  { href: "/builder", label: "Builder" },
  { href: "/definitions", label: "Definitions" },
  { href: "/runs", label: "Runs" },
  { href: "/approvals", label: "Approvals" },
];

// The root layout is the app chrome only (header + nav). The page body is laid
// out by the route-group layouts: `(site)` centres content in a max-width
// column; `(builder)` fills the viewport for the full-bleed canvas.
export default function RootLayout({ children }: { children: React.ReactNode }) {
  const { apiPublicUrl } = serverConfig();
  return (
    <html lang="en">
      <body className="flex min-h-screen flex-col antialiased">
        <RuntimeConfigProvider value={{ apiPublicUrl }}>
          <header className="shrink-0 border-b">
            <div className="mx-auto flex max-w-6xl items-center gap-6 px-6 py-3">
              <Link href="/" className="font-semibold tracking-tight">
                agentloom
              </Link>
              <nav className="flex items-center gap-4 text-sm text-muted-foreground">
                {NAV.map((item) => (
                  <Link key={item.href} href={item.href} className="hover:text-foreground">
                    {item.label}
                  </Link>
                ))}
              </nav>
            </div>
          </header>
          {children}
          <Toaster />
        </RuntimeConfigProvider>
      </body>
    </html>
  );
}
