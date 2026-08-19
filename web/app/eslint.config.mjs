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
    // The serialization boundary (M17.2) and any pure lib code under src/lib
    // must not import React/Next. Enforced here so the boundary is a lint rule,
    // not a convention. (Seeded now; lib/graphdef lands in 17.2.)
    files: ["src/lib/graphdef/**/*.ts"],
    rules: {
      "no-restricted-imports": [
        "error",
        {
          paths: [
            { name: "react", message: "graphdef is a pure module: no React imports." },
            { name: "react-dom", message: "graphdef is a pure module: no React imports." },
          ],
          patterns: ["next/*", "@/components/*"],
        },
      ],
    },
  },
];

export default config;
