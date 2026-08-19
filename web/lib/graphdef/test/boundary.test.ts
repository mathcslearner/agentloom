// DoD-3 (belt-and-braces): graphdef is a pure module with zero React/UI imports.
// The primary enforcement is the eslint no-restricted-imports rule
// (eslint.config.mjs, run by `pnpm lint`) and the fact that none of those
// packages is a dependency (so an import would not even resolve). This test is a
// second, dependency-free line: it scans the module source for forbidden import
// specifiers and asserts package.json declares no UI dependency.
import { readdirSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

const here = dirname(fileURLToPath(import.meta.url));
const SRC = resolve(here, "../src");
const PKG = resolve(here, "../package.json");

const FORBIDDEN = [/(^|['"])react(['"/]|$)/, /(^|['"])react-dom/, /(^|['"])next(['"/]|$)/, /@xyflow\//, /server-only/];

function walk(dir: string): string[] {
  const out: string[] = [];
  for (const e of readdirSync(dir, { withFileTypes: true })) {
    const p = join(dir, e.name);
    if (e.isDirectory()) out.push(...walk(p));
    else if (e.name.endsWith(".ts")) out.push(p);
  }
  return out;
}

describe("serialization boundary is pure", () => {
  it("no source file imports React/Next/React Flow", () => {
    for (const file of walk(SRC)) {
      const text = readFileSync(file, "utf8");
      // Only inspect import/export-from specifiers.
      const specifiers = [...text.matchAll(/(?:import|export)[^;]*?from\s*['"]([^'"]+)['"]/g)].map((m) => m[1]!);
      for (const spec of specifiers) {
        for (const bad of FORBIDDEN) {
          expect(bad.test(spec), `${file} imports forbidden specifier ${spec}`).toBe(false);
        }
      }
    }
  });

  it("package.json declares no React/UI dependency", () => {
    const pkg = JSON.parse(readFileSync(PKG, "utf8")) as {
      dependencies?: Record<string, string>;
      devDependencies?: Record<string, string>;
    };
    const deps = Object.keys({ ...pkg.dependencies, ...pkg.devDependencies });
    for (const d of deps) {
      expect(/^react$|^react-dom$|^next$|^@xyflow\//.test(d), `unexpected UI dependency ${d}`).toBe(false);
    }
    // graphdef is a pure library — no runtime dependencies at all.
    expect(pkg.dependencies ?? {}).toEqual({});
  });
});
