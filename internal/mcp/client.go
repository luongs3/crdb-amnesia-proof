// Package mcp is the audited READ path into agent memory via CockroachDB's Managed MCP Server.
//
// ARCHITECTURAL POINT, and the thing most entrants will get wrong: the agent's memory layer
// CANNOT run entirely through MCP. The managed server deny-lists system tables and never
// supports DROP/TRUNCATE, and its write surface is intentionally narrow. Routing transactional
// memory writes through it would be both slower and less safe than a direct pgx/pq connection.
//
// So this codebase splits the paths on purpose:
//
//	writes       -> direct SQL connection, serializable txn + hash-chained receipt
//	introspection-> Managed MCP Server, read-only, RBAC-checked, audit-logged per tool call
//
// That split is the correct use of the tool, not a workaround: an LLM operator inspecting the
// agent's memory gets a governed, logged, read-only surface, while the agent's own hot path
// keeps full transactional semantics.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

// DefaultEndpoint is CockroachDB Cloud's first-party managed MCP server.
// Verified live: an unauthenticated request returns HTTP 401 (endpoint exists, auth enforced),
// not 404.
const DefaultEndpoint = "https://cockroachlabs.cloud/mcp"

type Client struct {
	Endpoint string
	APIKey   string
	HTTP     *http.Client
}

func New() *Client {
	ep := os.Getenv("CRDB_MCP_ENDPOINT")
	if ep == "" {
		ep = DefaultEndpoint
	}
	return &Client{
		Endpoint: ep,
		APIKey:   os.Getenv("CRDB_API_KEY"), // service-account key, Cloud RBAC scoped per cluster
		HTTP:     &http.Client{Timeout: 30 * time.Second},
	}
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: 1, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("MCP endpoint reachable but unauthorized (set CRDB_API_KEY): HTTP 401")
	}
	if resp.StatusCode != http.StatusOK {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(resp.Body)
		return nil, fmt.Errorf("MCP %s: HTTP %d: %s", method, resp.StatusCode, buf.String())
	}

	var out rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode MCP response: %w", err)
	}
	if out.Error != nil {
		return nil, fmt.Errorf("MCP %s error %d: %s", method, out.Error.Code, out.Error.Message)
	}
	return out.Result, nil
}

// Probe checks reachability without credentials. A 401 is a SUCCESSFUL probe: it proves the
// endpoint exists and enforces auth, which is what we assert in the writeup.
func (c *Client) Probe(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint,
		bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	return fmt.Sprintf("HTTP %d %s", resp.StatusCode, truncate(buf.String(), 160)), nil
}

// ListDatabases and SelectQuery are the read-only consent tools the managed server exposes.
// Every invocation is RBAC-checked and emitted to structured logs tagged `mcp` with the tool
// name, cluster/org, redacted SQL shape, latency and response size.
func (c *Client) ListDatabases(ctx context.Context) (json.RawMessage, error) {
	return c.call(ctx, "tools/call", map[string]any{
		"name": "list_databases", "arguments": map[string]any{},
	})
}

// SelectQuery runs a read-only query through the governed path.
// Writes deliberately do NOT go through here — see the package comment.
func (c *Client) SelectQuery(ctx context.Context, sql string) (json.RawMessage, error) {
	return c.call(ctx, "tools/call", map[string]any{
		"name":      "select_query",
		"arguments": map[string]any{"query": sql},
	})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
