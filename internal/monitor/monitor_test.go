package monitor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/syntr0x79/btcpay-gate/internal/payments"
	"github.com/syntr0x79/btcpay-gate/internal/rpc"
)

// fakeNode returns a scripted answer per poll, so a whole chain history can be
// replayed without a node.
type fakeNode struct {
	responses     []rpc.SinceBlock
	calls         []string // cursor passed on each call
	confirmations []int    // target_confirmations passed on each call
	err           error
}

func (f *fakeNode) ListSinceBlock(_ context.Context, blockHash string, targetConf int) (rpc.SinceBlock, error) {
	f.calls = append(f.calls, blockHash)
	f.confirmations = append(f.confirmations, targetConf)
	if f.err != nil {
		return rpc.SinceBlock{}, f.err
	}
	if len(f.responses) == 0 {
		return rpc.SinceBlock{LastBlock: "empty"}, nil
	}
	r := f.responses[0]
	if len(f.responses) > 1 {
		f.responses = f.responses[1:]
	}
	return r, nil
}

func newStore(t *testing.T) *payments.Store {
	t.Helper()
	st, err := payments.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func newInvoice(t *testing.T, st *payments.Store, amount, conf int64) payments.Invoice {
	t.Helper()
	inv := payments.Invoice{
		ID:           "inv-1",
		Address:      "bcrt1qaddr",
		AmountSat:    amount,
		RequiredConf: conf,
		Status:       payments.StatusPending,
		CreatedAt:    time.Now(),
		ExpiresAt:    time.Now().Add(time.Hour),
	}
	if err := st.CreateInvoice(context.Background(), inv); err != nil {
		t.Fatal(err)
	}
	return inv
}

func recv(addr, txid string, btc float64, conf int64) rpc.Transaction {
	return rpc.Transaction{Address: addr, Category: "receive", TxID: txid, AmountBTC: btc, Confirmations: conf}
}

func statusOf(t *testing.T, st *payments.Store, id string) payments.Status {
	t.Helper()
	inv, err := st.Invoice(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return inv.Status
}

func newMonitor(node Node, st *payments.Store) *Monitor {
	return New(node, st, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestPaymentProgressesToConfirmed(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	inv := newInvoice(t, st, 100_000, 2)

	node := &fakeNode{responses: []rpc.SinceBlock{
		{Transactions: []rpc.Transaction{recv(inv.Address, "tx1", 0.001, 0)}, LastBlock: "b1"},
		{Transactions: []rpc.Transaction{recv(inv.Address, "tx1", 0.001, 1)}, LastBlock: "b2"},
		{Transactions: []rpc.Transaction{recv(inv.Address, "tx1", 0.001, 2)}, LastBlock: "b3"},
	}}
	m := newMonitor(node, st)

	mustPoll(t, m, ctx)
	if got := statusOf(t, st, inv.ID); got != payments.StatusSeen {
		t.Fatalf("after mempool: got %s, want seen", got)
	}

	mustPoll(t, m, ctx)
	if got := statusOf(t, st, inv.ID); got != payments.StatusConfirming {
		t.Fatalf("after 1 conf: got %s, want confirming", got)
	}

	mustPoll(t, m, ctx)
	if got := statusOf(t, st, inv.ID); got != payments.StatusConfirmed {
		t.Fatalf("after 2 conf: got %s, want confirmed", got)
	}
}

func TestCursorAdvancesAndIsReused(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	newInvoice(t, st, 100_000, 1)

	node := &fakeNode{responses: []rpc.SinceBlock{
		{LastBlock: "block-1"},
		{LastBlock: "block-2"},
	}}
	m := newMonitor(node, st)

	mustPoll(t, m, ctx)
	mustPoll(t, m, ctx)

	if node.calls[0] != "" {
		t.Fatalf("first call should start from genesis, got %q", node.calls[0])
	}
	if node.calls[1] != "block-1" {
		t.Fatalf("second call should resume from the stored cursor, got %q", node.calls[1])
	}

	cursor, err := st.Cursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cursor != "block-2" {
		t.Fatalf("cursor = %q, want block-2", cursor)
	}
}

func TestFailedPollDoesNotAdvanceTheCursor(t *testing.T) {
	// The whole replay guarantee rests on this: if a poll fails, the next one
	// must re-read the same range rather than skipping it.
	ctx := context.Background()
	st := newStore(t)
	newInvoice(t, st, 100_000, 1)

	node := &fakeNode{responses: []rpc.SinceBlock{{LastBlock: "block-1"}}}
	m := newMonitor(node, st)
	mustPoll(t, m, ctx)

	node.err = errors.New("node unreachable")
	if err := m.Poll(ctx); err == nil {
		t.Fatal("expected an error")
	}

	cursor, err := st.Cursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cursor != "block-1" {
		t.Fatalf("cursor moved to %q despite a failed poll", cursor)
	}
}

func TestReplayingTheSameBlockIsIdempotent(t *testing.T) {
	// Restarting mid-catch-up means core reports the same transactions again.
	// Counting them twice would double the amount received and settle an
	// invoice that was only half paid.
	ctx := context.Background()
	st := newStore(t)
	inv := newInvoice(t, st, 100_000, 1)

	same := rpc.SinceBlock{Transactions: []rpc.Transaction{recv(inv.Address, "tx1", 0.0006, 1)}, LastBlock: "b1"}
	node := &fakeNode{responses: []rpc.SinceBlock{same, same, same}}
	m := newMonitor(node, st)

	mustPoll(t, m, ctx)
	mustPoll(t, m, ctx)
	mustPoll(t, m, ctx)

	obs, err := st.Observations(ctx, inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 1 {
		t.Fatalf("got %d observations, want 1", len(obs))
	}
	if got := payments.Paid(obs); got != 60_000 {
		t.Fatalf("paid = %d, want 60000 — the same payment was counted more than once", got)
	}
	if got := statusOf(t, st, inv.ID); got != payments.StatusUnderpaid {
		t.Fatalf("got %s, want underpaid", got)
	}
}

func TestReorgMarksTheObservationRemoved(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	inv := newInvoice(t, st, 100_000, 1)

	node := &fakeNode{responses: []rpc.SinceBlock{
		{Transactions: []rpc.Transaction{recv(inv.Address, "tx1", 0.001, 1)}, LastBlock: "b1"},
		{Removed: []rpc.Transaction{recv(inv.Address, "tx1", 0.001, 0)}, LastBlock: "b2"},
	}}
	m := newMonitor(node, st)

	mustPoll(t, m, ctx)
	if got := statusOf(t, st, inv.ID); got != payments.StatusConfirmed {
		t.Fatalf("got %s, want confirmed", got)
	}

	// Confirmed is sticky by design, so use an invoice still in flight to see
	// the reorg transition itself.
	st2 := newStore(t)
	inv2 := newInvoice(t, st2, 100_000, 3)
	node2 := &fakeNode{responses: []rpc.SinceBlock{
		{Transactions: []rpc.Transaction{recv(inv2.Address, "tx1", 0.001, 1)}, LastBlock: "b1"},
		{Removed: []rpc.Transaction{recv(inv2.Address, "tx1", 0.001, 0)}, LastBlock: "b2"},
	}}
	m2 := newMonitor(node2, st2)

	mustPoll(t, m2, ctx)
	if got := statusOf(t, st2, inv2.ID); got != payments.StatusConfirming {
		t.Fatalf("got %s, want confirming", got)
	}
	mustPoll(t, m2, ctx)
	if got := statusOf(t, st2, inv2.ID); got != payments.StatusReorged {
		t.Fatalf("got %s, want reorged", got)
	}
}

func TestConflictedTransactionCountsAsRemoved(t *testing.T) {
	// A double spend: core still lists the original with 0 confirmations, but
	// walletconflicts says a competing transaction was mined. Waiting for it to
	// confirm would mean waiting forever.
	ctx := context.Background()
	st := newStore(t)
	inv := newInvoice(t, st, 100_000, 2)

	conflicted := recv(inv.Address, "tx1", 0.001, 0)
	conflicted.WalletConflicts = []string{"tx2"}

	node := &fakeNode{responses: []rpc.SinceBlock{
		{Transactions: []rpc.Transaction{recv(inv.Address, "tx1", 0.001, 1)}, LastBlock: "b1"},
		{Transactions: []rpc.Transaction{conflicted}, LastBlock: "b2"},
	}}
	m := newMonitor(node, st)

	mustPoll(t, m, ctx)
	if got := statusOf(t, st, inv.ID); got != payments.StatusConfirming {
		t.Fatalf("got %s, want confirming", got)
	}

	mustPoll(t, m, ctx)
	if got := statusOf(t, st, inv.ID); got != payments.StatusReorged {
		t.Fatalf("got %s, want reorged", got)
	}
}

func TestUnknownAddressesAreIgnored(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	inv := newInvoice(t, st, 100_000, 1)

	node := &fakeNode{responses: []rpc.SinceBlock{{
		Transactions: []rpc.Transaction{
			recv("bcrt1qsomeoneelse", "tx9", 5, 10),
			{Address: inv.Address, Category: "send", TxID: "tx8", AmountBTC: -1, Confirmations: 3},
		},
		LastBlock: "b1",
	}}}
	m := newMonitor(node, st)
	mustPoll(t, m, ctx)

	obs, err := st.Observations(ctx, inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 0 {
		t.Fatalf("got %d observations, want 0", len(obs))
	}
	if got := statusOf(t, st, inv.ID); got != payments.StatusPending {
		t.Fatalf("got %s, want pending", got)
	}
}

func TestAmountsAreConvertedWithoutLosingSatoshis(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	inv := newInvoice(t, st, 10_000_000, 1)

	// 0.1 BTC is not representable exactly as a float; truncation here loses a
	// satoshi and turns an exact payment into an underpayment.
	node := &fakeNode{responses: []rpc.SinceBlock{
		{Transactions: []rpc.Transaction{recv(inv.Address, "tx1", 0.1, 1)}, LastBlock: "b1"},
	}}
	m := newMonitor(node, st)
	mustPoll(t, m, ctx)

	obs, err := st.Observations(ctx, inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := payments.Paid(obs); got != 10_000_000 {
		t.Fatalf("paid = %d, want 10000000", got)
	}
	if got := statusOf(t, st, inv.ID); got != payments.StatusConfirmed {
		t.Fatalf("got %s, want confirmed", got)
	}
}

func mustPoll(t *testing.T, m *Monitor, ctx context.Context) {
	t.Helper()
	if err := m.Poll(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestCursorLagsTheTipSoConfirmationsKeepUpdating(t *testing.T) {
	// Regression test for a bug the unit tests could not see and the regtest
	// suite caught immediately: with target_confirmations=1, core returns the
	// tip as `lastblock`. The cursor then sits at the block containing the
	// payment, that payment is never reported again, and its confirmation
	// count freezes — an invoice requiring three confirmations waits forever
	// at one.
	ctx := context.Background()
	st := newStore(t)
	newInvoice(t, st, 100_000, 3)

	node := &fakeNode{responses: []rpc.SinceBlock{{LastBlock: "b1"}}}
	m := newMonitor(node, st)
	mustPoll(t, m, ctx)

	if len(node.confirmations) != 1 {
		t.Fatalf("expected one call, got %d", len(node.confirmations))
	}
	if node.confirmations[0] <= 1 {
		t.Fatalf("target_confirmations = %d; the cursor must lag the tip, or confirmations stop updating",
			node.confirmations[0])
	}
	if node.confirmations[0] < 3 {
		t.Fatalf("window %d is smaller than an invoice's required_conf of 3",
			node.confirmations[0])
	}
}

func TestWindowIsConfigurable(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	node := &fakeNode{responses: []rpc.SinceBlock{{LastBlock: "b1"}}}
	m := newMonitor(node, st).WithWindow(50)
	mustPoll(t, m, ctx)

	if node.confirmations[0] != 50 {
		t.Fatalf("target_confirmations = %d, want 50", node.confirmations[0])
	}
}
