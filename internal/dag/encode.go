package dag

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// Encode renders a definition in canonical form: fixed field order,
// optional fields omitted when empty, map keys sorted, no HTML escaping,
// compact output. The one exception is the `ui` subtree, which is spliced
// into the document byte-for-byte exactly as Decode captured it (ADR-003).
// Encoding the same Definition twice yields identical bytes.
func Encode(def *Definition) ([]byte, error) {
	if def == nil {
		return nil, errors.New("dag: Encode called with nil definition")
	}
	var buf bytes.Buffer
	buf.WriteString(`{"schema_version":`)
	buf.WriteString(strconv.Itoa(def.SchemaVersion))
	buf.WriteString(`,"name":`)
	if err := writeJSON(&buf, def.Name); err != nil {
		return nil, err
	}
	if def.Description != "" {
		buf.WriteString(`,"description":`)
		if err := writeJSON(&buf, def.Description); err != nil {
			return nil, err
		}
	}
	if def.OnFailure != "" {
		buf.WriteString(`,"on_failure":`)
		if err := writeJSON(&buf, def.OnFailure); err != nil {
			return nil, err
		}
	}
	if def.MaxWallClock != "" {
		buf.WriteString(`,"max_wall_clock":`)
		if err := writeJSON(&buf, def.MaxWallClock); err != nil {
			return nil, err
		}
	}
	if def.BudgetUSD != nil {
		buf.WriteString(`,"budget_usd":`)
		if err := writeJSON(&buf, *def.BudgetUSD); err != nil {
			return nil, err
		}
	}
	if def.OnBudgetExceeded != "" {
		buf.WriteString(`,"on_budget_exceeded":`)
		if err := writeJSON(&buf, def.OnBudgetExceeded); err != nil {
			return nil, err
		}
	}
	if len(def.Params) > 0 {
		buf.WriteString(`,"params":`)
		if err := writeJSON(&buf, def.Params); err != nil {
			return nil, err
		}
	}
	buf.WriteString(`,"steps":`)
	if err := writeSlice(&buf, def.Steps); err != nil {
		return nil, err
	}
	buf.WriteString(`,"edges":`)
	if err := writeSlice(&buf, def.Edges); err != nil {
		return nil, err
	}
	if len(def.UI) > 0 {
		if !json.Valid(def.UI) {
			return nil, errors.New("dag: Encode: ui block is not valid JSON")
		}
		buf.WriteString(`,"ui":`)
		buf.Write(def.UI) // verbatim splice; never re-marshaled
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// writeSlice writes a slice as a JSON array, rendering nil as [] so the
// required steps/edges keys are always arrays.
func writeSlice[T any](buf *bytes.Buffer, s []T) error {
	if s == nil {
		buf.WriteString("[]")
		return nil
	}
	return writeJSON(buf, s)
}

// writeJSON appends the no-escape JSON encoding of v to buf.
func writeJSON(buf *bytes.Buffer, v any) error {
	b, err := marshalNoEscape(v)
	if err != nil {
		return err
	}
	buf.Write(b)
	return nil
}

// marshalNoEscape is json.Marshal without HTML escaping, so CEL
// expressions like "output.n < 3" survive encoding unmangled.
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("dag: encode: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
