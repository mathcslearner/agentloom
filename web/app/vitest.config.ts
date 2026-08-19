import react from "@vitejs/plugin-react";
import { fileURLToPath } from "node:url";
import { defineConfig } from "vitest/config";

export default defineConfig({
  plugins: [react()],
  server: {
    fs: {
      // The inspector tests read the committed Go goldens under
      // internal/api/testdata (repo root, outside web/) — the exact API
      // contract they render (ticket 18.3). Allow the monorepo root so the
      // Vite fs sandbox does not deny them.
      allow: [fileURLToPath(new URL("../../..", import.meta.url))],
    },
  },
  resolve: {
    alias: {
      "@": fileURLToPath(new URL("./src", import.meta.url)),
      // `server-only` throws when imported outside a React Server Component
      // build; in unit tests we exercise the server modules directly, so map it
      // to a no-op. (The build still enforces the boundary — this is test-only.)
      "server-only": fileURLToPath(new URL("./test/stubs/server-only.ts", import.meta.url)),
    },
  },
  test: {
    include: ["test/**/*.test.{ts,tsx}"],
    environment: "jsdom",
    globals: true,
    setupFiles: ["test/setup.ts"],
  },
});
