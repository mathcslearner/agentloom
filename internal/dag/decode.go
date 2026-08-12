package dag

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
)

// Decode parses a workflow definition document per ADR-003: strictly —
// unknown fields, wrong types, unknown enum values, and missing required
// keys are all reported with the JSON path of the offender, joined into
// one error. The `ui` subtree is the single exception: it is kept verbatim
// and never inspected beyond being a JSON object.
//
// Decode gates on two things before anything else, each reported alone:
// the serialized-size limit (ADR-003; an oversized document is rejected
// before any parsing), and schema_version (a missing, non-integer, or
// unsupported version means the rest of the document cannot be
// meaningfully interpreted).
//
// Decode enforces the codec-level contract only. Graph-semantic rules —
// ID uniqueness and syntax, endpoint existence, required per-type config
// fields, limits — are structural validation (ticket 1.3).
func Decode(data []byte) (*Definition, error) {
	var errs errList
	if len(data) > MaxDefinitionBytes {
		errs.errs = append(errs.errs, &ValidationIssue{
			Code: CodeLimitExceeded, Severity: SeverityError,
			Msg: fmt.Sprintf("definition is %d bytes (max %d)", len(data), MaxDefinitionBytes),
		})
		return nil, errs.join()
	}
	if !json.Valid(data) {
		var v any
		err := json.Unmarshal(data, &v)
		errs.add("", "invalid JSON: %v", err)
		return nil, errs.join()
	}
	if jt := jsonTypeOf(data); jt != "object" {
		errs.add("", "expected object, got %s", jt)
		return nil, errs.join()
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		errs.add("", "%v", err)
		return nil, errs.join()
	}

	verRaw, ok := fields["schema_version"]
	if !ok {
		errs.add("schema_version", "required field is missing")
		return nil, errs.join()
	}
	ver, ok := decodeInt(verRaw, "schema_version", &errs)
	if !ok {
		return nil, errs.join()
	}
	if ver != CurrentSchemaVersion {
		errs.add("schema_version", "unsupported schema_version %d (this engine supports %d)", ver, CurrentSchemaVersion)
		return nil, errs.join()
	}

	def := &Definition{SchemaVersion: ver}
	if raw, ok := fields["name"]; ok {
		def.Name, _ = decodeString(raw, "name", &errs)
	} else {
		errs.add("name", "required field is missing")
	}
	if raw, ok := fields["description"]; ok {
		def.Description, _ = decodeString(raw, "description", &errs)
	}
	if raw, ok := fields["on_failure"]; ok {
		if s, sok := decodeString(raw, "on_failure", &errs); sok {
			def.OnFailure = FailurePolicy(s)
			if !slices.Contains(failurePolicies, def.OnFailure) {
				errs.add("on_failure", "unknown failure policy %q (expected one of: %s)", s, joinEnum(failurePolicies))
			}
		}
	}
	if raw, ok := fields["params"]; ok {
		def.Params = decodeParams(raw, &errs)
	}
	if raw, ok := fields["steps"]; ok {
		def.Steps = decodeSteps(raw, &errs)
	} else {
		errs.add("steps", "required field is missing")
	}
	if raw, ok := fields["edges"]; ok {
		def.Edges = decodeEdges(raw, &errs)
	} else {
		errs.add("edges", "required field is missing")
	}
	if raw, ok := fields["ui"]; ok {
		if jt := jsonTypeOf(raw); jt != "object" {
			errs.add("ui", "expected object, got %s", jt)
		} else {
			def.UI = raw // verbatim, never inspected
		}
	}

	topLevel := map[string]bool{
		"schema_version": true, "name": true, "description": true,
		"on_failure": true, "params": true, "steps": true, "edges": true,
		"ui": true,
	}
	for _, k := range sortedKeys(fields) {
		if !topLevel[k] {
			errs.add(k, "unknown field")
		}
	}

	if err := errs.join(); err != nil {
		return nil, err
	}
	return def, nil
}

// decodeParams decodes the top-level params declaration map.
func decodeParams(raw json.RawMessage, errs *errList) map[string]ParamSpec {
	m, ok := decodeObjectMap(raw, "params", errs)
	if !ok || len(m) == 0 {
		// An empty params object decodes to nil, matching Encode's omission
		// of empty params, so decode→encode→decode stays lossless.
		return nil
	}
	params := make(map[string]ParamSpec, len(m))
	for _, key := range sortedKeys(m) {
		path := "params." + key
		var spec ParamSpec
		strictUnmarshal(m[key], &spec, path, errs)
		if spec.Type == "" {
			errs.add(path+".type", "required field is missing")
		} else if !slices.Contains(paramTypes, spec.Type) {
			errs.add(path+".type", "unknown param type %q (expected one of: %s)", string(spec.Type), joinEnum(paramTypes))
		}
		params[key] = spec
	}
	return params
}

// decodeSteps decodes the steps array.
func decodeSteps(raw json.RawMessage, errs *errList) []Step {
	items, ok := decodeArray(raw, "steps", errs)
	if !ok {
		return nil
	}
	steps := make([]Step, 0, len(items))
	for i, item := range items {
		steps = append(steps, decodeStep(item, fmt.Sprintf("steps[%d]", i), errs))
	}
	return steps
}

// decodeStep decodes one step envelope and dispatches its config to the
// struct registered for the step type.
func decodeStep(raw json.RawMessage, path string, errs *errList) Step {
	var step Step
	m, ok := decodeObjectMap(raw, path, errs)
	if !ok {
		return step
	}
	if idRaw, present := m["id"]; present {
		step.ID, _ = decodeString(idRaw, path+".id", errs)
	} else {
		errs.add(path+".id", "required field is missing")
	}
	typeKnown := false
	if typeRaw, present := m["type"]; present {
		if s, sok := decodeString(typeRaw, path+".type", errs); sok {
			step.Type = StepType(s)
			if _, registered := stepConfigTypes[step.Type]; registered {
				typeKnown = true
			} else {
				errs.add(path+".type", "unknown step type %q", s)
			}
		}
	} else {
		errs.add(path+".type", "required field is missing")
	}
	if cfgRaw, present := m["config"]; present && typeKnown {
		step.Config = decodeStepConfig(step.Type, cfgRaw, path+".config", errs)
	}
	if retryRaw, present := m["retry"]; present {
		step.Retry = decodeRetry(retryRaw, path+".retry", errs)
	}
	if timeoutRaw, present := m["timeout"]; present {
		step.Timeout, _ = decodeString(timeoutRaw, path+".timeout", errs)
	}
	for _, k := range sortedKeys(m) {
		switch k {
		case "id", "type", "config", "retry", "timeout":
		default:
			errs.add(path+"."+k, "unknown field")
		}
	}
	return step
}

// decodeRetry decodes a step's retry policy (ADR-006). The codec level
// enforces shape and closed enums — unknown fields, mistyped values, an
// unknown jitter mode or error class; the numeric and duration bounds
// (and which classes `retry_on` may name) are structural validation
// (Validate), mirroring the config split between 1.2 and 1.3.
func decodeRetry(raw json.RawMessage, path string, errs *errList) *RetryPolicy {
	var rp RetryPolicy
	strictUnmarshal(raw, &rp, path, errs)
	if rp.Jitter != "" && !slices.Contains(jitterModes, rp.Jitter) {
		errs.add(path+".jitter", "unknown jitter mode %q (expected one of: %s)", string(rp.Jitter), joinEnum(jitterModes))
	}
	for i, c := range rp.RetryOn {
		if !slices.Contains(errorClasses, c) {
			errs.add(fmt.Sprintf("%s.retry_on[%d]", path, i), "unknown error class %q (expected one of: %s)", string(c), joinEnum(errorClasses))
		}
	}
	return &rp
}

// decodeStepConfig decodes a step's config into the typed struct for its
// step type, then applies per-type normalization and enum checks.
func decodeStepConfig(st StepType, raw json.RawMessage, path string, errs *errList) StepConfig {
	if jt := jsonTypeOf(raw); jt != "object" {
		errs.add(path, "expected object, got %s", jt)
		return nil
	}
	cfg := stepConfigTypes[st]()
	strictUnmarshal(raw, cfg, path, errs)
	switch c := cfg.(type) {
	case *ToolConfig:
		compactRaw(&c.Input)
	case *BranchConfig:
		compactRaw(&c.Input)
	case *EchoConfig:
		compactRaw(&c.Input)
	case *JoinConfig:
		if c.Mode != "" && c.Mode != JoinAll && c.Mode != JoinAny {
			errs.add(path+".mode", "unknown join mode %q (expected %q or %q)", string(c.Mode), JoinAll, JoinAny)
		}
	}
	return cfg
}

// DecodeStepConfig decodes one step's raw config JSON into the typed
// struct registered for its step type, with the same strictness as Decode
// (unknown fields rejected, per-type normalization applied). It exists for
// the execution layer (M4), which reads step type and config separately
// from a run's materialized graph copy (run_steps rows, ADR-004) rather
// than through a whole definition document. A nil or empty raw config
// returns (nil, nil) — the typed spelling of an absent config key.
func DecodeStepConfig(st StepType, raw json.RawMessage) (StepConfig, error) {
	if _, registered := stepConfigTypes[st]; !registered {
		return nil, &DecodeError{Path: "config", Msg: fmt.Sprintf("unknown step type %q", string(st))}
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var errs errList
	cfg := decodeStepConfig(st, raw, "config", &errs)
	if !errs.empty() {
		return nil, fmt.Errorf("invalid step config:\n%w", errors.Join(errs.errs...))
	}
	return cfg, nil
}

// decodeEdges decodes the edges array.
func decodeEdges(raw json.RawMessage, errs *errList) []Edge {
	items, ok := decodeArray(raw, "edges", errs)
	if !ok {
		return nil
	}
	edges := make([]Edge, 0, len(items))
	for i, item := range items {
		edges = append(edges, decodeEdge(item, fmt.Sprintf("edges[%d]", i), errs))
	}
	return edges
}

// decodeEdge decodes one edge and normalizes an absent type to EdgeNormal.
func decodeEdge(raw json.RawMessage, path string, errs *errList) Edge {
	var edge Edge
	m, ok := decodeObjectMap(raw, path, errs)
	if !ok {
		return edge
	}
	for _, key := range []string{"from", "to"} {
		if _, present := m[key]; !present {
			errs.add(path+"."+key, "required field is missing")
		}
	}
	strictUnmarshal(raw, &edge, path, errs)
	switch edge.Type {
	case "":
		edge.Type = EdgeNormal
	case EdgeNormal, EdgeLoop:
	default:
		errs.add(path+".type", "unknown edge type %q (expected %q or %q)", string(edge.Type), EdgeNormal, EdgeLoop)
	}
	return edge
}

// strictUnmarshal decodes a JSON object into the struct pointed to by dst,
// reporting unknown fields (recursively, all of them) and type mismatches
// with paths rooted at path.
func strictUnmarshal(raw json.RawMessage, dst any, path string, errs *errList) {
	if jt := jsonTypeOf(raw); jt != "object" {
		errs.add(path, "expected object, got %s", jt)
		return
	}
	checkUnknownFields(raw, reflect.TypeOf(dst).Elem(), path, errs)
	if err := json.Unmarshal(raw, dst); err != nil {
		addUnmarshalErr(errs, path, err)
	}
}

// rawMessageType marks the opaque-payload fields checkUnknownFields must
// not descend into.
var rawMessageType = reflect.TypeOf(json.RawMessage(nil))

// checkUnknownFields walks raw alongside the Go type it will decode into
// and records every JSON object key that has no corresponding field. It
// reports nothing else: type mismatches are left to json.Unmarshal (via
// addUnmarshalErr), and malformed shapes are simply skipped here.
func checkUnknownFields(raw json.RawMessage, t reflect.Type, path string, errs *errList) {
	if t == rawMessageType {
		return
	}
	switch t.Kind() {
	case reflect.Pointer:
		checkUnknownFields(raw, t.Elem(), path, errs)
	case reflect.Struct:
		m, ok := rawObject(raw)
		if !ok {
			return
		}
		known := jsonFieldTypes(t)
		for _, k := range sortedKeys(m) {
			ft, isKnown := known[k]
			if !isKnown {
				errs.add(path+"."+k, "unknown field")
				continue
			}
			checkUnknownFields(m[k], ft, path+"."+k, errs)
		}
	case reflect.Slice, reflect.Array:
		if jsonTypeOf(raw) != "array" {
			return
		}
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) != nil {
			return
		}
		for i, item := range items {
			checkUnknownFields(item, t.Elem(), fmt.Sprintf("%s[%d]", path, i), errs)
		}
	case reflect.Map:
		m, ok := rawObject(raw)
		if !ok {
			return
		}
		for _, k := range sortedKeys(m) {
			checkUnknownFields(m[k], t.Elem(), path+"."+k, errs)
		}
	default:
		// Scalar (or interface) leaf: nothing to descend into.
	}
}

// jsonFieldTypes maps a struct's JSON field names to their Go types.
func jsonFieldTypes(t reflect.Type) map[string]reflect.Type {
	out := make(map[string]reflect.Type, t.NumField())
	for i := range t.NumField() {
		f := t.Field(i)
		if f.PkgPath != "" { // unexported
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}
		out[name] = f.Type
	}
	return out
}

// addUnmarshalErr translates a json.Unmarshal error into a path-qualified
// DecodeError.
func addUnmarshalErr(errs *errList, path string, err error) {
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) {
		p := path
		if te.Field != "" {
			p = path + "." + te.Field
		}
		errs.add(p, "expected %s, got %s", jsonNameOfGoType(te.Type), te.Value)
		return
	}
	errs.add(path, "%v", err)
}

// jsonNameOfGoType names the JSON type a Go type decodes from, for error
// messages.
func jsonNameOfGoType(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Pointer:
		return jsonNameOfGoType(t.Elem())
	case reflect.String:
		return "string"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer"
	case reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Bool:
		return "boolean"
	case reflect.Slice, reflect.Array:
		return "array"
	default:
		return "object"
	}
}

// decodeString decodes a JSON string with a path-qualified error.
func decodeString(raw json.RawMessage, path string, errs *errList) (string, bool) {
	if jt := jsonTypeOf(raw); jt != "string" {
		errs.add(path, "expected string, got %s", jt)
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		errs.add(path, "%v", err)
		return "", false
	}
	return s, true
}

// decodeInt decodes a JSON integer (rejecting fractional numbers) with a
// path-qualified error.
func decodeInt(raw json.RawMessage, path string, errs *errList) (int, bool) {
	if jt := jsonTypeOf(raw); jt != "number" {
		errs.add(path, "expected integer, got %s", jt)
		return 0, false
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		errs.add(path, "expected integer, got %s", bytes.TrimSpace(raw))
		return 0, false
	}
	return n, true
}

// decodeObjectMap decodes a JSON object into its raw fields with a
// path-qualified error when the value is not an object.
func decodeObjectMap(raw json.RawMessage, path string, errs *errList) (map[string]json.RawMessage, bool) {
	if jt := jsonTypeOf(raw); jt != "object" {
		errs.add(path, "expected object, got %s", jt)
		return nil, false
	}
	m, ok := rawObject(raw)
	if !ok {
		errs.add(path, "expected object")
		return nil, false
	}
	return m, true
}

// decodeArray decodes a JSON array into its raw elements with a
// path-qualified error when the value is not an array.
func decodeArray(raw json.RawMessage, path string, errs *errList) ([]json.RawMessage, bool) {
	if jt := jsonTypeOf(raw); jt != "array" {
		errs.add(path, "expected array, got %s", jt)
		return nil, false
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		errs.add(path, "%v", err)
		return nil, false
	}
	return items, true
}

// rawObject decodes a JSON value into an object field map, reporting
// whether the value was an object.
func rawObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	if jsonTypeOf(raw) != "object" {
		return nil, false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, false
	}
	return m, true
}

// jsonTypeOf reports the JSON type of a raw value by its first
// non-whitespace byte. The input must already be known-valid JSON.
func jsonTypeOf(raw []byte) string {
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		case '{':
			return "object"
		case '[':
			return "array"
		case '"':
			return "string"
		case 't', 'f':
			return "boolean"
		case 'n':
			return "null"
		default:
			return "number"
		}
	}
	return "empty value"
}

// compactRaw normalizes an opaque payload to compact JSON so canonical
// re-encoding is byte-stable (the `ui` block is exempt: it stays verbatim).
func compactRaw(raw *json.RawMessage) {
	if len(*raw) == 0 {
		return
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, *raw); err == nil {
		*raw = json.RawMessage(bytes.Clone(buf.Bytes()))
	}
}

// sortedKeys returns m's keys in sorted order, for deterministic error
// reporting and iteration.
func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}

// joinEnum renders enum values for error messages.
func joinEnum[T ~string](vals []T) string {
	strs := make([]string, len(vals))
	for i, v := range vals {
		strs[i] = string(v)
	}
	return strings.Join(strs, ", ")
}
