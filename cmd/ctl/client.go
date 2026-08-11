package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mathcslearner/agentloom/internal/api"
)

// client is a thin HTTP client over internal/api's wire contract.
type client struct {
	base string
	hc   *http.Client
}

func newClient(base string) *client {
	return &client{
		base: strings.TrimRight(base, "/"),
		hc:   &http.Client{Timeout: 30 * time.Second},
	}
}

// apiError is a non-2xx response decoded into the API's error envelope.
type apiError struct {
	Status int
	Detail api.ErrorDetail
}

func (e *apiError) Error() string {
	msg := e.Detail.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	if e.Detail.Code != "" {
		return fmt.Sprintf("api error (%s): %s", e.Detail.Code, msg)
	}
	return fmt.Sprintf("api error (HTTP %d): %s", e.Status, msg)
}

// submitRun POSTs a run submission.
func (c *client) submitRun(ctx context.Context, req api.SubmitRunRequest) (api.SubmitRunResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return api.SubmitRunResponse{}, fmt.Errorf("encoding request: %w", err)
	}
	var resp api.SubmitRunResponse
	err = c.do(ctx, http.MethodPost, "/v1/runs", body, &resp)
	return resp, err
}

// getRun GETs one run's status tree.
func (c *client) getRun(ctx context.Context, runID string) (api.RunResponse, error) {
	var resp api.RunResponse
	err := c.do(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(runID), nil, &resp)
	return resp, err
}

// do runs one request, decoding 2xx into out and anything else into an
// *apiError.
func (c *client) do(ctx context.Context, method, path string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("calling %s: %w", c.base+path, err)
	}
	defer res.Body.Close() //nolint:errcheck // read-side close
	if res.StatusCode < 200 || res.StatusCode > 299 {
		e := &apiError{Status: res.StatusCode}
		var envelope api.ErrorBody
		if err := json.NewDecoder(res.Body).Decode(&envelope); err == nil {
			e.Detail = envelope.Error
		}
		return e
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(res.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	return nil
}
