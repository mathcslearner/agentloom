package exec

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/plugin"
)

// ApprovalRequest is what HumanApprovalExecutor produces for the engine's
// park path (ticket 15.2, ADR-017): the rendered, validated content of a
// pending approval. Unlike an ordinary executor output it is never persisted
// as the step's result — completeAwaitHuman decodes it, writes the approvals
// row, and parks the step (the decision, 15.3, becomes the step's output).
// The engine and the executor are the only readers, so the shape is a plain
// internal handoff (the planner "executor produces, engine applies" pattern).
type ApprovalRequest struct {
	Title       string          `json:"title"`
	Description string          `json:"description,omitempty"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	// AllowedDecisions is the resolved decision set (never empty — the
	// [approve, reject] default is applied here).
	AllowedDecisions []string `json:"allowed_decisions"`
	AllowEdit        bool     `json:"allow_edit,omitempty"`
	// EditSchema is the compiled-and-verified edit constraint (nil = any JSON
	// edit accepted); carried verbatim so 15.3 enforces it at decide time.
	EditSchema json.RawMessage `json:"edit_schema,omitempty"`
	// Timeout is the wait before the timeout policy (15.4) fires; 0 = wait
	// indefinitely. Validated positive here so the engine adds it to now
	// without re-parsing.
	Timeout time.Duration `json:"timeout,omitempty"`
}

// HumanApprovalExecutor runs human_approval steps (ticket 15.2, ADR-017). It
// does the deterministic pre-flight work — decode the rendered config,
// resolve the default decision set, compile the edit schema, parse the
// timeout — and hands the result to the engine, which parks the step without
// a lease. It calls nothing external: side_effectful is set so the cache
// middleware treats the park as uncacheable (a decision is never served from
// cache), but it names no resource and estimates no cost, so the limiter,
// budget, and window stages all bypass it structurally.
type HumanApprovalExecutor struct{}

// Type implements Executor.
func (HumanApprovalExecutor) Type() string { return string(dag.StepHumanApproval) }

// PluginManifest implements SelfDescribing (ticket 8.1). Side-effectful (a
// pending approval is an outward-facing request for a human decision) and
// uncacheable; not cost-bearing (the config carries no spend).
func (HumanApprovalExecutor) PluginManifest() plugin.Manifest {
	return builtinManifest(dag.StepHumanApproval, "1.0.0",
		"Park the run for a human decision (approve / reject / edit).",
		plugin.Capabilities{SideEffectful: true})
}

// Execute decodes and validates the rendered approval config. It never
// blocks: the wait happens in the engine (the step parks without a lease).
// A corrupt config, an uncompilable edit schema, or an unparseable timeout is
// a permanent failure — config was validated at submit time (15.1), so a
// failure here is corrupt stored state or version skew, and no retry can fix
// it (the same stance as configAs).
func (HumanApprovalExecutor) Execute(_ context.Context, sc StepContext) (Output, error) {
	c, err := configAs[*dag.HumanApprovalConfig](sc)
	if err != nil {
		return Output{}, err // *InvalidConfigError → permanent
	}
	if c == nil || c.Title == "" {
		return Output{}, Permanentf("human_approval: missing required field %q", "title")
	}

	// Resolve the decision set: empty config means the engine default
	// [approve, reject] (ADR-017). Rendered as strings for the approvals row.
	allowed := c.AllowedDecisions
	if len(allowed) == 0 {
		allowed = []dag.ApprovalDecision{dag.ApprovalApprove, dag.ApprovalReject}
	}
	decisions := make([]string, len(allowed))
	for i, d := range allowed {
		decisions[i] = string(d)
	}

	// Compile the edit schema now so an uncompilable schema fails permanently
	// before the step parks — rather than parking and failing at every decide
	// attempt (the 11.3 output_format claim-pre-flight precedent). The
	// compiled artifact is not persisted; 15.3 recompiles at decide time.
	if len(c.EditSchema) > 0 {
		if _, err := CompileEditSchema(c.EditSchema); err != nil {
			return Output{}, Permanentf("human_approval: edit_schema does not compile: %v", err)
		}
	}

	// Parse the timeout (validated parseable/positive at submit time; a
	// corrupt materialized value is permanent). 0 = wait indefinitely.
	var timeout time.Duration
	if c.Timeout != "" {
		d, perr := time.ParseDuration(c.Timeout)
		if perr != nil {
			return Output{}, Permanentf("human_approval: corrupt timeout %q: %v", c.Timeout, perr)
		}
		timeout = d
	}

	req := ApprovalRequest{
		Title:            c.Title,
		Description:      c.Description,
		Payload:          c.Payload,
		AllowedDecisions: decisions,
		AllowEdit:        c.AllowEdit,
		EditSchema:       c.EditSchema,
		Timeout:          timeout,
	}
	data, merr := json.Marshal(req)
	if merr != nil {
		// Payload is opaque author JSON that already round-tripped through
		// decode; a marshal failure here would be a bug, treated permanent.
		return Output{}, Permanentf("human_approval: marshaling approval request: %v", merr)
	}
	return Output{Data: data}, nil
}

// CompileEditSchema compiles an edit-constraint JSON Schema, mirroring the
// json_schema validator's compilation (self-contained schemas; no external
// $ref resolution). It is the shared compiler for both the 15.2 park
// pre-flight (schema must compile before the step parks) and 15.3's decide
// path (validate an edited payload against the returned schema).
func CompileEditSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	const loc = "mem://human-approval/edit-schema.json"
	if err := c.AddResource(loc, doc); err != nil {
		return nil, err
	}
	return c.Compile(loc)
}
