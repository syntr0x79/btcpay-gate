package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// recorded is one RPC call as bitcoind saw it.
type recorded struct {
	Method string `json:"method"`
	Params []any  `json:"params"`
}

// fakeNode answers each method from a script and records what it was asked.
// Keyed by method because a single CreateWatchOnlyWallet may issue two calls.
type fakeNode struct {
	replies map[string]*Error
	calls   []recorded
}

func newFakeNode(t *testing.T, replies map[string]*Error) (*Client, *fakeNode) {
	t.Helper()
	f := &fakeNode{replies: replies}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req recorded
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("unparsable request body %q", body)
		}
		f.calls = append(f.calls, req)

		resp := map[string]any{"result": nil, "error": nil}
		if e, ok := f.replies[req.Method]; ok && e != nil {
			resp["error"] = e
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "u", "p"), f
}

func (f *fakeNode) methods() []string {
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.Method)
	}
	return out
}

// A wallet that does not exist yet is created with load_on_startup set, so
// that a later restart of the node brings it back without the gateway.
func TestCreateWatchOnlyWalletSetsLoadOnStartup(t *testing.T) {
	c, f := newFakeNode(t, nil)

	if err := c.CreateWatchOnlyWallet(context.Background(), "payments"); err != nil {
		t.Fatalf("CreateWatchOnlyWallet: %v", err)
	}
	if got := f.methods(); len(got) != 1 || got[0] != "createwallet" {
		t.Fatalf("calls = %v, want exactly [createwallet]", got)
	}
	params := f.calls[0].Params
	if len(params) != 7 {
		t.Fatalf("createwallet got %d params, want 7: %v", len(params), params)
	}
	if params[6] != true {
		t.Errorf("createwallet load_on_startup = %v, want true", params[6])
	}
}

// The wallet is on disk from an earlier run. Nothing guarantees it is still in
// core's startup list — on shop-pl it was not — so the fallback has to put it
// back rather than merely loading it for this process's lifetime.
func TestExistingWalletIsLoadedWithLoadOnStartup(t *testing.T) {
	c, f := newFakeNode(t, map[string]*Error{
		"createwallet": {Code: -4, Message: "Wallet file verification failed. Database already exists."},
	})

	if err := c.CreateWatchOnlyWallet(context.Background(), "payments"); err != nil {
		t.Fatalf("CreateWatchOnlyWallet: %v", err)
	}
	if got := f.methods(); len(got) != 2 || got[1] != "loadwallet" {
		t.Fatalf("calls = %v, want [createwallet loadwallet]", got)
	}
	params := f.calls[1].Params
	if len(params) != 2 {
		t.Fatalf("loadwallet got %d params, want 2 (name, load_on_startup): %v", len(params), params)
	}
	if params[1] != true {
		t.Errorf("loadwallet load_on_startup = %v, want true", params[1])
	}
}

// The trap this whole change exists for. Once the wallet is in core's startup
// list the node loads it on its own, and the gateway's own loadwallet then
// answers -35. Treating that as fatal put the gateway in a restart loop on
// 2026-09-22 and is why -wallet= "could not be used" to fix the outage.
func TestAlreadyLoadedWalletIsNotAnError(t *testing.T) {
	c, _ := newFakeNode(t, map[string]*Error{
		"createwallet": {Code: -4, Message: "Database already exists."},
		"loadwallet":   {Code: -35, Message: `Wallet "payments" is already loaded.`},
	})

	if err := c.CreateWatchOnlyWallet(context.Background(), "payments"); err != nil {
		t.Fatalf("an already-loaded wallet was treated as fatal: %v", err)
	}
}

// Tolerating -35 must not turn into tolerating everything: a wallet that
// cannot be opened at all has to stop the gateway, not be started around.
func TestUnloadableWalletStillFails(t *testing.T) {
	c, _ := newFakeNode(t, map[string]*Error{
		"createwallet": {Code: -4, Message: "Database already exists."},
		"loadwallet": {Code: -4, Message: "Wallet loading failed. Prune: last wallet " +
			"synchronisation goes beyond pruned data. You need to -reindex."},
	})

	err := c.CreateWatchOnlyWallet(context.Background(), "payments")
	if err == nil {
		t.Fatal("a wallet that core refuses to open was reported as success")
	}
	var re *Error
	if !errors.As(err, &re) || re.Code != -4 {
		t.Errorf("err = %v, want the -4 from loadwallet", err)
	}
}

func TestIsWalletNotLoaded(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"code -18", &Error{Code: -18, Message: "Requested wallet does not exist or is not loaded"}, true},
		// Call wraps every error with the method name, so the classifier only
		// ever sees the wrapped form in production.
		{"wrapped -18", fmt.Errorf("listsinceblock: %w", &Error{Code: -18, Message: "not loaded"}), true},
		{"other rpc error", &Error{Code: -4, Message: "nope"}, false},
		{"nil", nil, false},
		{"plain error", errors.New("context deadline exceeded"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsWalletNotLoaded(tc.err); got != tc.want {
				t.Errorf("IsWalletNotLoaded(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// Measured against bitcoin core 28.0 on regtest: a freshly imported active
// descriptor is widened to the wallet's keypool regardless of the range asked
// for, and every later import must then cover that wider range. ActiveRangeEnd
// is how the caller finds out what it has to cover.
func TestActiveRangeEndReadsTheWidestActiveExternalRange(t *testing.T) {
	c, srv := newRangeNode(t, `{"result":{"wallet_name":"payments","descriptors":[
		{"desc":"wpkh(tpubA/0/*)#aaa","active":true,"internal":false,"range":[0,999],"next":3},
		{"desc":"wpkh(tpubA/1/*)#bbb","active":true,"internal":true,"range":[0,4999],"next":0},
		{"desc":"wpkh(tpubOLD/0/*)#ccc","active":false,"internal":false,"range":[0,9999],"next":0}
	]}}`)
	defer srv.Close()

	got, err := c.ActiveRangeEnd(context.Background())
	if err != nil {
		t.Fatalf("ActiveRangeEnd: %v", err)
	}
	// 999, not 4999 and not 9999: the internal (change) descriptor is not
	// where invoices are paid, and an inactive one is not watched at all.
	if got != 999 {
		t.Errorf("ActiveRangeEnd = %d, want 999", got)
	}
}

// A wallet the gateway has just created has no descriptor yet. Zero, not an
// error: "nothing is watched" is a normal state at first start.
func TestActiveRangeEndIsZeroOnAFreshWallet(t *testing.T) {
	c, srv := newRangeNode(t, `{"result":{"wallet_name":"payments","descriptors":[]}}`)
	defer srv.Close()

	got, err := c.ActiveRangeEnd(context.Background())
	if err != nil {
		t.Fatalf("ActiveRangeEnd: %v", err)
	}
	if got != 0 {
		t.Errorf("ActiveRangeEnd = %d, want 0", got)
	}
}

// An unranged descriptor has no range at all. It cannot be the gateway's
// (Open refuses those), but it can sit in the same wallet, and indexing into
// a missing field is how a nil panic reaches production.
func TestActiveRangeEndIgnoresUnrangedDescriptors(t *testing.T) {
	c, srv := newRangeNode(t, `{"result":{"descriptors":[
		{"desc":"pkh(tpubA/0/0)#aaa","active":true,"internal":false}
	]}}`)
	defer srv.Close()

	got, err := c.ActiveRangeEnd(context.Background())
	if err != nil {
		t.Fatalf("ActiveRangeEnd: %v", err)
	}
	if got != 0 {
		t.Errorf("ActiveRangeEnd = %d, want 0", got)
	}
}

func newRangeNode(t *testing.T, body string) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req recorded
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &req)
		if req.Method != "listdescriptors" {
			t.Errorf("ActiveRangeEnd called %q, want listdescriptors", req.Method)
		}
		_, _ = w.Write([]byte(body))
	}))
	return New(srv.URL, "u", "p"), srv
}
