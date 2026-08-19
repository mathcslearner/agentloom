import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  // The typed clients are workspace packages consumed from source (no prebuilt
  // dist), so Next must transpile them.
  transpilePackages: ["@agentloom/api-client", "@agentloom/engine-client"],
  // Fail the production build on type or lint errors rather than shipping them.
  typescript: { ignoreBuildErrors: false },
  eslint: { ignoreDuringBuilds: false },
};

export default nextConfig;
