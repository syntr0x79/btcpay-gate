//go:build regtest

// Integration tests against a real bitcoind on regtest.
//
// Behind a build tag so `go test ./...` needs nothing installed. Run with:
//
//	make test-regtest
//
// What these cover that the unit tests cannot: that our reading of core's
// actual responses is right. listsinceblock's shape, what removed[] contains
// after a reorg, how confirmations behave when a block is invalidated — all of
// that is assumed by the unit tests and verified here.
package regtest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/syntr0x79/btcpay-gate/internal/api"
	"github.com/syntr0x79/btcpay-gate/internal/monitor"
	"github.com/syntr0x79/btcpay-gate/internal/payments"
	"github.com/syntr0x79/btcpay-gate/internal/rpc"
	"github.com/syntr0x79/btcpay-gate/internal/wallet"
	"github.com/syntr0x79/btcpay-gate/internal/webhook"
)

const (
	rpcUser = "paygate"
	rpcPass = "paygate"

	// A well-known regtest xpub. Public material only — this repository must
	// never contain anything that can spend.
	testXpub = "tpubD6NzVbkrYhZ4XgiXtGrdW5XDAPFCL9h7we1vwNCpn8tGbBcgfVYjXyhWo4E1xkh56hjod1RhGjxbaTLV3X4FyWuejifB9jusQ46QzG87VKp"
)

// branch keeps runs from colliding. The node may be shared and long-lived,
// while every test starts with an empty database — so a fixed derivation path
// would make run N+1 observe run N's payments on the same address, and the
// amounts would silently be wrong. A per-run branch gives each run its own
// address space out of the same xpub.
var branchCounter atomic.Int64

func descriptorFor(t *testing.T) string {
	t.Helper()
	branch := (time.Now().UnixNano()/1e6)%1_000_000 + branchCounter.Add(1)
	return fmt.Sprintf("wpkh(%s/%d/*)", testXpub, branch)
}

// --- harness ---------------------------------------------------------------

type harness struct {
	node      *rpc.Client // wallet-scoped, watch-only
	miner     *rpc.Client // wallet-scoped, has coins
	nodeRaw   *rpc.Client // node-level calls
	store     *payments.Store
	mon       *monitor.Monitor
	srv       *api.Server
	minerAddr string
}

func rpcURL() string {
	if u := os.Getenv("BITCOIND_RPC_URL"); u != "" {
		return u
	}
	return "http://127.0.0.1:18443"
}

func setup(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	raw := rpc.New(rpcURL(), rpcUser, rpcPass)
	if _, err := raw.GetBlockchainInfo(ctx); err != nil {
		t.Skipf("bitcoind not reachable (%v) — run `make up` first", err)
	}

	// Unique wallet names keep tests independent even though they share a node.
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	minerName := "miner" + suffix
	watchName := "watch" + suffix

	// The miner wallet holds keys and coins; it stands in for "the rest of the
	// world" that pays our invoices.
	if err := raw.Call(ctx, nil, "createwallet", minerName); err != nil {
		t.Fatalf("create miner wallet: %v", err)
	}
	miner := rpc.New(rpcURL(), rpcUser, rpcPass, rpc.WithWallet(minerName))

	minerAddr, err := miner.GetNewAddress(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// 101 blocks: coinbase output matures after 100, so this is the minimum
	// that leaves something spendable.
	if _, err := miner.GenerateToAddress(ctx, 101, minerAddr); err != nil {
		t.Fatal(err)
	}

	node := rpc.New(rpcURL(), rpcUser, rpcPass, rpc.WithWallet(watchName))
	w, err := wallet.Open(ctx, node, wallet.Config{
		Descriptor:   descriptorFor(t),
		WalletName:   watchName,
		InitialRange: 20,
	})
	if err != nil {
		t.Fatalf("open watch-only wallet: %v", err)
	}

	store, err := payments.Open(filepath.Join(t.TempDir(), "regtest.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	return &harness{
		node:      node,
		miner:     miner,
		nodeRaw:   raw,
		store:     store,
		mon:       monitor.New(node, store, time.Second, log),
		srv:       api.New(store, w, api.Config{}, log),
		minerAddr: minerAddr,
	}
}

func (h *harness) createInvoice(t *testing.T, amountSat, conf int64) (id, address string) {
	t.Helper()
	body := fmt.Sprintf(`{"amount_sat": %d, "required_conf": %d}`, amountSat, conf)
	r := httptest.NewRequest("POST", "/invoices", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.srv.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("create invoice: status %d, body %s", w.Code, w.Body)
	}
	var out struct {
		ID      string `json:"id"`
		Address string `json:"address"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.ID, out.Address
}

func (h *harness) poll(t *testing.T) {
	t.Helper()
	if err := h.mon.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
}

// waitFor polls until the invoice reaches the expected status.
//
// A mempool transaction does not appear in the watch-only wallet the instant
// sendtoaddress returns — the node relays it between wallets asynchronously.
// A fixed sleep would paper over that and make the suite slow and flaky at
// once; waiting on the condition is both faster and honest about what is
// being awaited.
func (h *harness) waitFor(t *testing.T, id string, want payments.Status) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last payments.Status
	for time.Now().Before(deadline) {
		h.poll(t)
		last = h.status(t, id)
		if last == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("invoice %s stuck at %s, want %s", id, last, want)
}

func (h *harness) status(t *testing.T, id string) payments.Status {
	t.Helper()
	inv, err := h.store.Invoice(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return inv.Status
}

func (h *harness) mine(t *testing.T, n int) []string {
	t.Helper()
	hashes, err := h.miner.GenerateToAddress(context.Background(), n, h.minerAddr)
	if err != nil {
		t.Fatal(err)
	}
	return hashes
}

func (h *harness) pay(t *testing.T, address string, btc float64) string {
	t.Helper()
	txid, err := h.miner.SendToAddress(context.Background(), address, btc)
	if err != nil {
		t.Fatal(err)
	}
	return txid
}

// --- tests -----------------------------------------------------------------

func TestPaymentReachesConfirmed(t *testing.T) {
	h := setup(t)
	id, addr := h.createInvoice(t, 100_000, 2)

	h.poll(t)
	if got := h.status(t, id); got != payments.StatusPending {
		t.Fatalf("got %s, want pending", got)
	}

	h.pay(t, addr, 0.001)
	h.waitFor(t, id, payments.StatusSeen)

	h.mine(t, 1)
	h.waitFor(t, id, payments.StatusConfirming)

	h.mine(t, 1)
	h.waitFor(t, id, payments.StatusConfirmed)
}

func TestUnderpaymentIsReported(t *testing.T) {
	h := setup(t)
	id, addr := h.createInvoice(t, 100_000, 1)

	h.pay(t, addr, 0.0006) // 60_000 sat
	h.mine(t, 1)
	h.waitFor(t, id, payments.StatusUnderpaid)

	// The rest arrives and settles it.
	h.pay(t, addr, 0.0004)
	h.mine(t, 1)
	h.waitFor(t, id, payments.StatusConfirmed)
}

// TestReorgTakesThePaymentBack is the test this project exists for.
//
// A payment is mined, the invoice starts confirming, and then the block
// holding it is invalidated. Core reports the transaction in listsinceblock's
// removed[], and the invoice must stop counting money that is no longer in the
// chain.
func TestReorgTakesThePaymentBack(t *testing.T) {
	h := setup(t)
	ctx := context.Background()
	id, addr := h.createInvoice(t, 100_000, 3)

	h.pay(t, addr, 0.001)
	blocks := h.mine(t, 1)
	h.poll(t)

	if got := h.status(t, id); got != payments.StatusConfirming {
		t.Fatalf("got %s, want confirming before the reorg", got)
	}

	// Rewind the tip. The block — and the payment in it — leave the chain.
	if err := h.nodeRaw.InvalidateBlock(ctx, blocks[0]); err != nil {
		t.Fatalf("invalidateblock: %v", err)
	}
	h.poll(t)

	got := h.status(t, id)
	if got == payments.StatusConfirming || got == payments.StatusConfirmed {
		t.Fatalf("got %s after the block was invalidated — the invoice is still counting money that left the chain", got)
	}
	if got != payments.StatusReorged && got != payments.StatusSeen {
		t.Fatalf("got %s, want reorged or seen", got)
	}

	obs, err := h.store.Observations(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range obs {
		if !o.Removed && o.Confirmations > 0 {
			t.Fatalf("observation %s still claims %d confirmations after the reorg", o.TxID, o.Confirmations)
		}
	}
}

func TestReorgedPaymentRecoversWhenMinedAgain(t *testing.T) {
	h := setup(t)
	ctx := context.Background()
	id, addr := h.createInvoice(t, 100_000, 1)

	h.pay(t, addr, 0.001)
	blocks := h.mine(t, 1)
	h.poll(t)

	if err := h.nodeRaw.InvalidateBlock(ctx, blocks[0]); err != nil {
		t.Fatal(err)
	}
	h.poll(t)

	// Mine the replacement chain to a different address. Mining to the same one
	// in the same second reproduces the invalidated block bit for bit, and core
	// rejects it because that hash is already marked invalid — a regtest-only
	// trap that looks like a node failure.
	fresh, err := h.miner.GetNewAddress(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.miner.GenerateToAddress(ctx, 2, fresh); err != nil {
		t.Fatalf("mine replacement chain: %v", err)
	}
	h.poll(t)

	if got := h.status(t, id); got != payments.StatusConfirmed {
		t.Fatalf("got %s, want confirmed once the payment was mined again", got)
	}
}

func TestRestartResumesFromTheCursor(t *testing.T) {
	// Blocks that arrive while the service is down must not be missed. This is
	// the property polling buys and a push notification does not.
	h := setup(t)
	ctx := context.Background()
	id, addr := h.createInvoice(t, 100_000, 2)

	h.poll(t)
	before, err := h.store.Cursor(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// "Service is down" — no polling at all while this happens.
	h.pay(t, addr, 0.001)
	h.mine(t, 3)

	// It comes back and catches up in a single pass.
	h.poll(t)

	if got := h.status(t, id); got != payments.StatusConfirmed {
		t.Fatalf("got %s, want confirmed — blocks mined during downtime were missed", got)
	}
	after, err := h.store.Cursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Fatal("cursor did not advance")
	}
}

func TestWebhookFiresOnStatusChange(t *testing.T) {
	h := setup(t)
	id, addr := h.createInvoice(t, 100_000, 1)

	var (
		mu       sync.Mutex
		received []string
		secret   = "integration-secret"
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !webhook.Verify([]byte(secret), r.Header.Get("X-Webhook-Timestamp"), body,
			r.Header.Get("X-Webhook-Signature"), time.Now(), 5*time.Minute) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var ev webhook.Event
		json.Unmarshal(body, &ev)
		mu.Lock()
		received = append(received, ev.Status)
		mu.Unlock()
	}))
	defer srv.Close()

	sender := webhook.New(h.store.DB(), webhook.Config{URL: srv.URL, Secret: secret},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	h.pay(t, addr, 0.001)
	h.waitFor(t, id, payments.StatusSeen)
	if err := sender.DeliverDue(context.Background()); err != nil {
		t.Fatal(err)
	}

	h.mine(t, 1)
	h.waitFor(t, id, payments.StatusConfirmed)
	if err := sender.DeliverDue(context.Background()); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) < 2 {
		t.Fatalf("received %v, want at least seen and confirmed", received)
	}
	if received[len(received)-1] != string(payments.StatusConfirmed) {
		t.Fatalf("last webhook was %q, want confirmed", received[len(received)-1])
	}
}

func TestAddressesAreNeverReused(t *testing.T) {
	h := setup(t)
	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		_, addr := h.createInvoice(t, 50_000, 1)
		if seen[addr] {
			t.Fatalf("address %s handed out twice", addr)
		}
		seen[addr] = true
	}
}

func TestWatchOnlyWalletCannotSpend(t *testing.T) {
	// The security claim in the README, asserted rather than described: the
	// node holding our descriptor must be unable to move funds.
	h := setup(t)
	_, addr := h.createInvoice(t, 50_000, 1)
	h.pay(t, addr, 0.001)
	h.mine(t, 1)
	h.poll(t)

	_, err := h.node.SendToAddress(context.Background(), h.minerAddr, 0.0005)
	if err == nil {
		t.Fatal("the watch-only wallet was able to spend — it must hold no private keys")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "private keys") &&
		!strings.Contains(strings.ToLower(err.Error()), "disabled") {
		t.Logf("spend refused with: %v", err)
	}
}
