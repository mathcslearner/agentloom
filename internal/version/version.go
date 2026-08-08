// Package version exposes the build version of agentloom binaries.
// The default "dev" is overridden at build time via
// -ldflags "-X github.com/mathcslearner/agentloom/internal/version.Version=v1.2.3".
package version

// Version is the semantic version of the current build.
var Version = "dev"
