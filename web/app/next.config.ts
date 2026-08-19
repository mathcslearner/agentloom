import type { NextConfig } from "next";

// A minimal structural view of the bits of the webpack config we touch (the
// `webpack` type is not a direct dependency; Next bundles it).
interface WebpackResolveConfig {
  resolve?: { extensionAlias?: Record<string, string[]> };
}

const nextConfig: NextConfig = {
  // The typed clients + the serialization boundary are workspace packages
  // consumed from source (no prebuilt dist), so Next must transpile them.
  transpilePackages: ["@agentloom/api-client", "@agentloom/engine-client", "@agentloom/graphdef"],
  // Fail the production build on type or lint errors rather than shipping them.
  typescript: { ignoreBuildErrors: false },
  eslint: { ignoreDuringBuilds: false },
  webpack(config: WebpackResolveConfig) {
    // The source packages use NodeNext `.js` extension specifiers on their
    // relative imports (e.g. `./to-flow.js` → `to-flow.ts`). api-client's are
    // type-only (erased), but graphdef's are runtime, so webpack must map a
    // `.js` request onto the `.ts`/`.tsx` source when bundling the package.
    config.resolve ??= {};
    config.resolve.extensionAlias = {
      ".js": [".ts", ".tsx", ".js"],
      ".mjs": [".mts", ".mjs"],
      ...config.resolve.extensionAlias,
    };
    return config;
  },
};

export default nextConfig;
