// Package monitor keeps the local view of invoices in step with the chain.
//
// One loop, one query: listsinceblock from a persisted cursor. There is no
// push notification anywhere in this design, and that is the point — see the
// note on Run.
package monitor

import (
	"context"
	"log/slog"
	"time"

	"github.com/syntr0x79/btcpay-gate/internal/payments"
	"github.com/syntr0x79/btcpay-gate/internal/rpc"
)

type Node interface {
	ListSinceBlock(ctx context.Context, blockHash string, minConf int) (rpc.SinceBlock, error)
}

// Reporter is told the outcome of every poll. The monitor is the only thing in
// the process that learns whether the chain is readable, so it is the only
// thing that can keep the health endpoint honest.
type Reporter interface {
	PollSucceeded()
	PollFailed(err error)
}

// nopReporter keeps Poll free of nil checks. A monitor without a reporter is a
// valid monitor — the ledger does not depend on anyone watching.
type nopReporter struct{}

func (nopReporter) PollSucceeded()       {}
func (nopReporter) PollFailed(err error) {}

type Monitor struct {
	node     Node
	store    *payments.Store
	interval time.Duration
	window   int
	log      *slog.Logger
	now      func() time.Time
	health   Reporter
}

// DefaultWindow is how far behind the tip the cursor is kept, in blocks.
//
// This is not a tuning knob, it is a correctness requirement. listsinceblock
// reports transactions that arrived *after* the cursor; once the cursor passes
// the block holding a payment, that payment stops being reported and its
// confirmation count in our database freezes forever. An invoice needing three
// confirmations would sit at one until someone noticed.
//
// Passing target_confirmations makes core return a `lastblock` that many
// blocks back from the tip, so payments stay inside the window until they are
// deeper than anything we care about. The window must exceed the largest
// required_conf any invoice can ask for.
const DefaultWindow = 12

func New(node Node, store *payments.Store, interval time.Duration, log *slog.Logger) *Monitor {
	return &Monitor{
		node:     node,
		store:    store,
		interval: interval,
		window:   DefaultWindow,
		log:      log,
		now:      time.Now,
		health:   nopReporter{},
	}
}

// WithHealth routes poll outcomes to the shared health state read by /healthz
// and /metrics.
func (m *Monitor) WithHealth(r Reporter) *Monitor {
	if r != nil {
		m.health = r
	}
	return m
}

// WithWindow overrides how far the cursor lags the tip. Raise it above
// DefaultWindow if invoices may require more confirmations than that.
func (m *Monitor) WithWindow(blocks int) *Monitor {
	if blocks > 0 {
		m.window = blocks
	}
	return m
}

// Run polls until the context is cancelled.
//
// Why polling and not ZMQ: a push notification is delivered once. A process
// that is restarting, or wedged, or being deployed when a block arrives never
// learns about it — and nothing in the system knows a payment was missed. A
// cursor into the chain is replayable by construction: on start we ask "what
// happened since this block?" and core answers with everything, including the
// blocks we were not running for.
//
// The cost is latency bounded by the poll interval, which against a ten-minute
// block time is irrelevant.
func (m *Monitor) Run(ctx context.Context) error {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	// Catch up immediately rather than waiting out the first tick — a restart
	// should not add a poll interval of blindness.
	if err := m.Poll(ctx); err != nil {
		m.log.Error("initial poll failed", "err", err)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := m.Poll(ctx); err != nil {
				// A failed poll is not fatal: the cursor did not move, so the
				// next tick re-reads the same range. Transient node trouble
				// costs latency, never correctness.
				m.log.Error("poll failed", "err", err)
			}
		}
	}
}

// Poll performs one catch-up pass. Exported so tests and the regtest suite can
// drive it deterministically instead of waiting for a ticker.
//
// Every outcome is reported to the health state, including the ones Run only
// logs: a log line is what the last outage produced, and nobody read it.
func (m *Monitor) Poll(ctx context.Context) error {
	err := m.poll(ctx)
	if err != nil {
		m.health.PollFailed(err)
		return err
	}
	m.health.PollSucceeded()
	return nil
}

func (m *Monitor) poll(ctx context.Context) error {
	cursor, err := m.store.Cursor(ctx)
	if err != nil {
		return err
	}

	// The window keeps the cursor behind the tip so a payment keeps being
	// reported until it is deeper than any threshold we track — see
	// DefaultWindow. Unconfirmed transactions are returned regardless of this
	// value, which is what lets an invoice reach `seen` from the mempool.
	since, err := m.node.ListSinceBlock(ctx, cursor, m.window)
	if err != nil {
		return err
	}

	update := payments.Update{Cursor: since.LastBlock}

	for _, tx := range since.Transactions {
		obs, ok, err := m.observation(ctx, tx, false)
		if err != nil {
			return err
		}
		if ok {
			update.Observations = append(update.Observations, obs)
		}
	}

	// removed[] is the reorg signal: these transactions were in the chain at
	// the previous cursor and are not in it now. Core only fills this in when
	// include_removed is set, which is why the RPC call hard-codes it.
	for _, tx := range since.Removed {
		obs, ok, err := m.observation(ctx, tx, true)
		if err != nil {
			return err
		}
		if ok {
			update.Observations = append(update.Observations, obs)
		}
	}

	changes, err := m.store.Apply(ctx, update, m.now())
	if err != nil {
		return err
	}
	for _, c := range changes {
		m.log.Info("invoice status changed", "invoice", c.InvoiceID, "from", c.From, "to", c.To)
	}
	return nil
}

// observation maps one wallet transaction onto an invoice. Transactions for
// addresses we do not know about are ignored rather than treated as an error:
// the wallet may legitimately contain other activity.
func (m *Monitor) observation(ctx context.Context, tx rpc.Transaction, removed bool) (payments.ObservationUpdate, bool, error) {
	if !tx.IsReceive() {
		return payments.ObservationUpdate{}, false, nil
	}
	inv, err := m.store.InvoiceByAddress(ctx, tx.Address)
	if err == payments.ErrNotFound {
		return payments.ObservationUpdate{}, false, nil
	}
	if err != nil {
		return payments.ObservationUpdate{}, false, err
	}

	conf := tx.Confirmations
	// A transaction with a mined conflict is gone for good, even though it may
	// still appear in transactions[] with confirmations 0. Treating it as
	// merely unconfirmed would leave the invoice waiting forever for a payment
	// that has already been replaced.
	if len(tx.WalletConflicts) > 0 {
		removed = true
	}
	if conf < 0 {
		// Core reports negative confirmations for conflicted transactions.
		removed = true
		conf = 0
	}

	return payments.ObservationUpdate{
		InvoiceID: inv.ID,
		Observation: payments.Observation{
			TxID:          tx.TxID,
			Vout:          tx.Vout,
			AmountSat:     int64(tx.Amount()),
			Confirmations: conf,
			Removed:       removed,
		},
	}, true, nil
}
