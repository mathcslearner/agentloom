// ESLint flat config for @agentloom/graphdef.
//
// graphdef is the serialization boundary (ADR-019): a PURE data-transformation
// module with ZERO React/UI imports. That boundary is a lint rule here, not a
// convention — no-restricted-imports fails the build on any React/Next/React
// Flow specifier. (The package's own dependency graph is the second line of
// defence: none of those packages is a dependency, so the import would not
// resolve either.) See test/boundary.test.ts for the belt-and-braces check.
import js from "@eslint/js";
import tsParser from "@typescript-eslint/parser";

const boundary = {
  paths: [
    { name: "react", message: "graphdef is a pure module: no React imports (ADR-019)." },
    { name: "react-dom", message: "graphdef is a pure module: no React imports (ADR-019)." },
    { name: "server-only", message: "graphdef is environment-agnostic: no server-only." },
  ],
  patterns: ["next", "next/*", "@xyflow/*", "@/components/*", "react-dom/*"],
};

export default [
  { ignores: ["dist/**", "node_modules/**", "src/generated/**"] },
  js.configs.recommended,
  {
    files: ["**/*.ts", "**/*.mts"],
    languageOptions: {
      parser: tsParser,
      parserOptions: { ecmaVersion: "latest", sourceType: "module" },
    },
    rules: {
      "no-restricted-imports": ["error", boundary],
      // The TS parser handles module/type syntax; the base no-unused-vars rule
      // false-positives on type-only constructs, so silence it (typecheck is the
      // real unused-symbol gate via `tsc --noEmit`).
      "no-unused-vars": "off",
      "no-undef": "off",
    },
  },
];
