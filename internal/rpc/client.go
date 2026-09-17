// Package rpc is a minimal JSON-RPC client for bitcoin-core.
//
// Deliberately hand-written rather than pulled from a library: the surface we
// need is six calls, and a dependency that wraps the whole RPC API would be
// more code to audit than the code it replaces — in a service that touches
// money, that trade is worth making explicitly.
package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Client struct {
	url      string
	user     string
	password string
	wallet   string // empty = node-level endpoint
	http     *http.Client
}

type Option func(*Client)

// WithWallet targets a named wallet, i.e. /wallet/<name>. Core requires this
// for every wallet-scoped call once more than one wallet is loaded.
func WithWallet(name string) Option { return func(c *Client) { c.wallet = name } }

func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

func New(url, user, password string, opts ...Option) *Client {
	c := &Client{
		url:      url,
		user:     user,
		password: password,
		http:     &http.Client{Timeout: 30 * time.Second},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      string `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("bitcoind error %d: %s", e.Code, e.Message) }

// Code exposes core's error code so callers can distinguish "wallet already
// exists" from "node is unreachable" without matching on message text.
func (e *rpcError) ErrorCode() int { return e.Code }

type response struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

func (c *Client) endpoint() string {
	if c.wallet == "" {
		return c.url
	}
	return c.url + "/wallet/" + c.wallet
}

// Call issues one JSON-RPC call and unmarshals result into out (may be nil).
func (c *Client) Call(ctx context.Context, out any, method string, params ...any) error {
	if params == nil {
		params = []any{}
	}
	body, err := json.Marshal(request{JSONRPC: "1.0", ID: "btcpay-gate", Method: method, Params: params})
	if err != nil {
		return fmt.Errorf("marshal %s: %w", method, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request %s: %w", method, err)
	}
	req.SetBasicAuth(c.user, c.password)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("call %s: %w", method, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read %s response: %w", method, err)
	}

	// Core answers 500 with a well-formed JSON-RPC error body, so the status
	// code alone is not the signal — decode first, and only fall back to the
	// status when the body is not JSON at all.
	var r response
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("call %s: http %d: %s", method, resp.StatusCode, truncate(raw, 200))
	}
	if r.Error != nil {
		return fmt.Errorf("%s: %w", method, r.Error)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(r.Result, out); err != nil {
		return fmt.Errorf("decode %s result: %w", method, err)
	}
	return nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
