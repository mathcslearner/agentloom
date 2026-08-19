import { dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { FlatCompat } from "@eslint/eslintrc";

const compat = new FlatCompat({ baseDirectory: dirname(fileURLToPath(import.meta.url)) });

const config = [
  ...compat.extends("next/core-web-vitals", "next/typescript"),
  {
    ignores: [".next/**", "node_modules/**", "playwright-report/**", "test-results/**", "next-env.d.ts"],
  },
  {
    // The serialization boundary landed in its own workspace package,
    // @agentloom/graphdef (web/lib/graphdef, ADR-019), whose own
    // eslint.config.mjs enforces the no-React/UI boundary. This block keeps the
    // same guard for any future *pure* helper code the app grows under src/lib
    // (server-only config and the api factories are exempt — they are app glue,
    // not pure logic — so the pattern is scoped to a `pure/` subdirectory).
    files: ["src/lib/pure/**/*.ts"],
    rules: {
      "no-restricted-imports": [
        "error",
        {
          paths: [
            { name: "react", message: "pure app-lib code: no React imports." },
            { name: "react-dom", message: "pure app-lib code: no React imports." },
          ],
          patterns: ["next/*", "@/components/*"],
        },
      ],
    },
  },
];

export default config;
