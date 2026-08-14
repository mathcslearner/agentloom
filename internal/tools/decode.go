package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// strictUnmarshal decodes a tool's validated args into a typed struct with
// DisallowUnknownFields — belt-and-suspenders behind the schema's
// additionalProperties: false, and the codebase's established decode
// discipline (dag.DecodeStepConfig). Empty or nil input decodes to the
// zero struct (the caller presence-checks required fields).
func strictUnmarshal(raw []byte, dst any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("unexpected trailing data after JSON value")
	}
	return nil
}

// decodeJSONValue decodes an arbitrary JSON payload into a Go value for
// gojq. Numbers decode to float64 (plain json.Unmarshal), which is gojq's
// expected input form. Empty or nil input is the JSON null value.
func decodeJSONValue(raw []byte) (any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// readAllLimited reads up to limit bytes from r, reporting whether the
// source exceeded the cap (the (limit+1)th byte existed). Used by
// http_request to bound response bodies.
func readAllLimited(r io.Reader, limit int64) ([]byte, bool, error) {
	buf, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(buf)) > limit {
		return buf[:limit], true, nil
	}
	return buf, false, nil
}
