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

	"github.com/spf13/cobra"

	"github.com/mathcslearner/agentloom/internal/api"
)

// client is a thin HTTP client over internal/api's wire contract.
type client struct {
	base string
	// key is the bearer credential presented on every request (empty =
	// anonymous, which every /v1 route rejects since ticket 6.2).
	key string
	hc  *http.Client
}

func newClient(base, key string) *client {
	return &client{
		base: strings.TrimRight(base, "/"),
		key:  key,
		hc:   &http.Client{Timeout: 30 * time.Second},
	}
}

// clientFromCmd builds the client from the root command's persistent
// --api and --key flags.
func clientFromCmd(cmd *cobra.Command) (*client, error) {
	base, err := cmd.Flags().GetString("api")
	if err != nil {
		return nil, err
	}
	key, err := cmd.Flags().GetString("key")
	if err != nil {
		return nil, err
	}
	return newClient(base, key), nil
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

// createKey POSTs a key-creation request (admin).
func (c *client) createKey(ctx context.Context, req api.CreateKeyRequest) (api.CreateKeyResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return api.CreateKeyResponse{}, fmt.Errorf("encoding request: %w", err)
	}
	var resp api.CreateKeyResponse
	err = c.do(ctx, http.MethodPost, "/v1/keys", body, &resp)
	return resp, err
}

// listKeys GETs every key's projection (admin; prefixes only).
func (c *client) listKeys(ctx context.Context) (api.ListKeysResponse, error) {
	var resp api.ListKeysResponse
	err := c.do(ctx, http.MethodGet, "/v1/keys", nil, &resp)
	return resp, err
}

// revokeKey DELETEs one key (admin; soft, idempotent — a 204 either way).
func (c *client) revokeKey(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/keys/"+url.PathEscape(id), nil, nil)
}

// submitRun POSTs a run submission; a non-empty idempotencyKey rides the
// Idempotency-Key header (ticket 6.5).
func (c *client) submitRun(ctx context.Context, req api.SubmitRunRequest, idempotencyKey string) (api.SubmitRunResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return api.SubmitRunResponse{}, fmt.Errorf("encoding request: %w", err)
	}
	var resp api.SubmitRunResponse
	err = c.do(ctx, http.MethodPost, "/v1/runs", body, &resp, func(r *http.Request) {
		if idempotencyKey != "" {
			r.Header.Set(api.IdempotencyKeyHeader, idempotencyKey)
		}
	})
	return resp, err
}

// getRun GETs one run's status tree.
func (c *client) getRun(ctx context.Context, runID string) (api.RunResponse, error) {
	var resp api.RunResponse
	err := c.do(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(runID), nil, &resp)
	return resp, err
}

// listRuns GETs one keyset page of runs (ticket 6.5). query is the
// pre-encoded query string ("" for the first unfiltered page).
func (c *client) listRuns(ctx context.Context, query url.Values) (api.ListRunsResponse, error) {
	path := "/v1/runs"
	if enc := query.Encode(); enc != "" {
		path += "?" + enc
	}
	var resp api.ListRunsResponse
	err := c.do(ctx, http.MethodGet, path, nil, &resp)
	return resp, err
}

// cancelRun POSTs the run-cancel op (ticket 6.5).
func (c *client) cancelRun(ctx context.Context, runID string) (api.CancelRunResponse, error) {
	var resp api.CancelRunResponse
	err := c.do(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(runID)+"/cancel", nil, &resp)
	return resp, err
}

// parkRun POSTs the run-park op (ticket 6.5).
func (c *client) parkRun(ctx context.Context, runID string) (api.ParkRunResponse, error) {
	var resp api.ParkRunResponse
	err := c.do(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(runID)+"/park", nil, &resp)
	return resp, err
}

// unparkRun POSTs the run-unpark op (ticket 6.5).
func (c *client) unparkRun(ctx context.Context, runID string) (api.UnparkRunResponse, error) {
	var resp api.UnparkRunResponse
	err := c.do(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(runID)+"/unpark", nil, &resp)
	return resp, err
}

// setBudget PATCHes a run's spend budget (ticket 10.3).
func (c *client) setBudget(ctx context.Context, runID string, budgetUSD float64) (api.SetBudgetResponse, error) {
	body, err := json.Marshal(api.SetBudgetRequest{BudgetUSD: &budgetUSD})
	if err != nil {
		return api.SetBudgetResponse{}, fmt.Errorf("encoding request: %w", err)
	}
	var resp api.SetBudgetResponse
	err = c.do(ctx, http.MethodPatch, "/v1/runs/"+url.PathEscape(runID)+"/budget", body, &resp)
	return resp, err
}

// requeueStep POSTs the dead-lettered-step requeue op (ticket 6.5).
func (c *client) requeueStep(ctx context.Context, runID, stepID string) (api.RequeueStepResponse, error) {
	var resp api.RequeueStepResponse
	err := c.do(ctx, http.MethodPost,
		"/v1/runs/"+url.PathEscape(runID)+"/steps/"+url.PathEscape(stepID)+"/requeue", nil, &resp)
	return resp, err
}

// listPlugins GETs the plugin catalog (ticket 8.1).
func (c *client) listPlugins(ctx context.Context) (api.ListPluginsResponse, error) {
	var resp api.ListPluginsResponse
	err := c.do(ctx, http.MethodGet, "/v1/plugins", nil, &resp)
	return resp, err
}

// bustCache POSTs a response-cache bust by namespace (ticket 9.6, admin).
func (c *client) bustCache(ctx context.Context, req api.CacheBustRequest) (api.CacheBustResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return api.CacheBustResponse{}, fmt.Errorf("encoding request: %w", err)
	}
	var resp api.CacheBustResponse
	err = c.do(ctx, http.MethodPost, "/v1/cache/bust", body, &resp)
	return resp, err
}

// cacheStats GETs the per-plugin cache counters (ticket 9.6, admin).
func (c *client) cacheStats(ctx context.Context) (api.CacheStatsResponse, error) {
	var resp api.CacheStatsResponse
	err := c.do(ctx, http.MethodGet, "/v1/cache/stats", nil, &resp)
	return resp, err
}

// do runs one request, decoding 2xx into out and anything else into an
// *apiError. Each mod, if any, edits the request before it is sent (e.g.
// extra headers).
func (c *client) do(ctx context.Context, method, path string, body []byte, out any, mods ...func(*http.Request)) error {
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
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	for _, mod := range mods {
		mod(req)
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
