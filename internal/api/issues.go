package api

import (
	"errors"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// DefinitionIssues flattens a dag.Decode or dag.Validate error into wire
// Issues. Both join their per-finding typed errors (*dag.DecodeError,
// *dag.ValidationIssue) with errors.Join, so this walks the unwrap tree
// and collects every leaf it recognizes. Unrecognized leaves (there are
// none today) surface as path-less error-severity issues rather than being
// dropped — a 400 must never hide its reason. Exported because ctl's local
// `validate` renders the same issue shape without an HTTP round trip.
func DefinitionIssues(err error) []Issue {
	var issues []Issue
	var walk func(error)
	walk = func(err error) {
		if err == nil {
			return
		}
		switch e := err.(type) { //nolint:errorlint // walking the tree, not matching one target
		case interface{ Unwrap() []error }:
			for _, sub := range e.Unwrap() {
				walk(sub)
			}
		case *dag.DecodeError:
			issues = append(issues, Issue{Severity: string(dag.SeverityError), Path: e.Path, Msg: e.Msg})
		case *dag.ValidationIssue:
			issues = append(issues, Issue{
				Code: string(e.Code), Severity: string(e.Severity), Path: e.Path, Msg: e.Msg,
			})
		default:
			if sub := errors.Unwrap(err); sub != nil {
				walk(sub)
				return
			}
			issues = append(issues, Issue{Severity: string(dag.SeverityError), Msg: err.Error()})
		}
	}
	walk(err)
	return issues
}

// ValidationIssues converts dag.Validate's full findings list (warnings
// included) into wire Issues.
func ValidationIssues(found []*dag.ValidationIssue) []Issue {
	issues := make([]Issue, 0, len(found))
	for _, i := range found {
		issues = append(issues, Issue{
			Code: string(i.Code), Severity: string(i.Severity), Path: i.Path, Msg: i.Msg,
		})
	}
	return issues
}
