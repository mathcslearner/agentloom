package loadgen

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

// apiClient is loadgen's minimal HTTP client over internal/api's wire contract.
// It differs from ctl's client in one way that matters under load: a transport
// tuned for many concurrent short requests (raised idle-conns-per-host), so a
// 60+/s submit stream reuses connections instead of churning them.
type apiClient struct {
	base string
	key  string
	hc   *http.Client
}

func newAPIClient(base, key string, submitTimeout time.Duration) *apiClient {
	tr := &http.Transport{
		MaxIdleConns:        512,
		MaxIdleConnsPerHost: 512,
		IdleConnTimeout:     90 * time.Second,
	}
	return &apiClient{
		base: strings.TrimRight(base, "/"),
		key:  key,
		hc:   &http.Client{Transport: tr, Timeout: submitTimeout},
	}
}

// httpResult is the outcome of one request: the status (0 on a transport
// error), the decoded error envelope (on non-2xx), and any transport error.
type httpResult struct {
	Status int
	Code   string // API error code on non-2xx, "" otherwise
	Err    error  // transport/timeout error (Status == 0)
}

func (c *apiClient) do(ctx context.Context, method, path string, body []byte, out any) httpResult {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return httpResult{Err: fmt.Errorf("building request: %w", err)}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	res, err := c.hc.Do(req)
	if err != nil {
		return httpResult{Err: err}
	}
	defer res.Body.Close() //nolint:errcheck // read-side close
	if res.StatusCode < 200 || res.StatusCode > 299 {
		r := httpResult{Status: res.StatusCode}
		var env api.ErrorBody
		if json.NewDecoder(res.Body).Decode(&env) == nil {
			r.Code = env.Error.Code
		}
		return r
	}
	if out != nil {
		if err := json.NewDecoder(res.Body).Decode(out); err != nil {
			return httpResult{Status: res.StatusCode, Err: fmt.Errorf("decoding response: %w", err)}
		}
	}
	return httpResult{Status: res.StatusCode}
}

// health probes GET /healthz.
func (c *apiClient) health(ctx context.Context) error {
	if r := c.do(ctx, http.MethodGet, "/healthz", nil, nil); r.Err != nil || r.Status != http.StatusOK {
		if r.Err != nil {
			return r.Err
		}
		return fmt.Errorf("healthz = %d", r.Status)
	}
	return nil
}

// registerDefinition ensures a definition is stored and returns a fresh
// version id for this campaign: it POSTs the definition (creating v1) and, if
// the name already exists (409), appends a new version — so every campaign
// targets a unique definition_id and the run-list reconciliation is exact.
func (c *apiClient) registerDefinition(ctx context.Context, name string, spec json.RawMessage) (string, error) {
	body, _ := json.Marshal(api.CreateDefinitionRequest{Definition: spec})
	var resp api.DefinitionResponse
	r := c.do(ctx, http.MethodPost, "/v1/definitions", body, &resp)
	switch {
	case r.Err != nil:
		return "", r.Err
	case r.Status == http.StatusCreated || r.Status == http.StatusOK:
		return resp.ID, nil
	case r.Status == http.StatusConflict:
		// Name exists — append a fresh version.
		var vresp api.DefinitionResponse
		vr := c.do(ctx, http.MethodPost, "/v1/definitions/"+url.PathEscape(name)+"/versions", body, &vresp)
		if vr.Err != nil {
			return "", vr.Err
		}
		if vr.Status != http.StatusCreated && vr.Status != http.StatusOK {
			return "", fmt.Errorf("append version %s = %d (%s)", name, vr.Status, vr.Code)
		}
		return vresp.ID, nil
	default:
		return "", fmt.Errorf("register %s = %d (%s)", name, r.Status, r.Code)
	}
}

// submitResult captures a submission's outcome for the taxonomy.
type submitResult struct {
	RunID  string
	Status int
	Code   string
	Err    error
	RTT    time.Duration
}

// submit POSTs a run (by definition_id or inline definition) and returns the
// outcome plus the client-measured round-trip time.
func (c *apiClient) submit(ctx context.Context, defID string, spec, params json.RawMessage) submitResult {
	req := api.SubmitRunRequest{Params: params}
	if defID != "" {
		req.DefinitionID = defID
	} else {
		req.Definition = spec
	}
	body, _ := json.Marshal(req)
	var resp api.SubmitRunResponse
	start := time.Now()
	r := c.do(ctx, http.MethodPost, "/v1/runs", body, &resp)
	rtt := time.Since(start)
	return submitResult{RunID: resp.RunID, Status: r.Status, Code: r.Code, Err: r.Err, RTT: rtt}
}

// getRun fetches one run's status tree.
func (c *apiClient) getRun(ctx context.Context, runID string) (api.RunResponse, int, error) {
	var resp api.RunResponse
	r := c.do(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(runID), nil, &resp)
	if r.Err != nil {
		return api.RunResponse{}, 0, r.Err
	}
	return resp, r.Status, nil
}

// listRunsByDefinition walks every run of one definition created at/after the
// campaign start, following keyset cursors to exhaustion. Used only in the
// final reconciliation sweep, so the O(runs) cost is paid once.
func (c *apiClient) listRunsByDefinition(ctx context.Context, defID string, createdAfter time.Time) ([]api.RunView, error) {
	var all []api.RunView
	cursor := ""
	for {
		q := url.Values{}
		q.Set("definition_id", defID)
		q.Set("created_after", createdAfter.UTC().Format(time.RFC3339Nano))
		q.Set("limit", "200")
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var resp api.ListRunsResponse
		r := c.do(ctx, http.MethodGet, "/v1/runs?"+q.Encode(), nil, &resp)
		if r.Err != nil {
			return all, r.Err
		}
		if r.Status != http.StatusOK {
			return all, fmt.Errorf("list runs = %d (%s)", r.Status, r.Code)
		}
		all = append(all, resp.Runs...)
		if resp.NextCursor == "" {
			return all, nil
		}
		cursor = resp.NextCursor
	}
}

// systemStats fetches the queue/outbox/DLQ snapshot behind the progress line
// and the quiescence check.
func (c *apiClient) systemStats(ctx context.Context) (api.SystemStatsResponse, error) {
	var resp api.SystemStatsResponse
	r := c.do(ctx, http.MethodGet, "/v1/system/stats", nil, &resp)
	if r.Err != nil {
		return api.SystemStatsResponse{}, r.Err
	}
	if r.Status != http.StatusOK {
		return api.SystemStatsResponse{}, fmt.Errorf("system stats = %d (%s)", r.Status, r.Code)
	}
	return resp, nil
}
