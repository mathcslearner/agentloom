package tools

// NewBuiltins assembles the registry of built-in tools both deployables
// share (ticket 8.7): http_request (configured from opts — allowlist,
// timeout, response cap) and json_transform (config-free). It is the one
// constructor cmd/worker and cmd/api call, so the plugin catalog the API
// lists matches the tools the fleet can execute.
//
// http_request is always registered; its allowlist governs whether it can
// reach anything (empty = deny all), so a deployment that configures no
// hosts still lists the tool but every call is blocked. json_transform is
// pure and needs no configuration.
func NewBuiltins(opts HTTPOptions) (*Registry, error) {
	return NewRegistry(
		NewHTTPRequest(opts),
		NewJSONTransform(),
	)
}
