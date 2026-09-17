package payments

import (
	"testing"
	"time"
)

var now = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

func invoice(status Status, amount, conf int64, expires time.Time) Invoice {
	return Invoice{
		ID:           "inv-1",
		Address:      "bcrt1qexample",
		AmountSat:    amount,
		RequiredConf: conf,
		Status:       status,
		CreatedAt:    now.Add(-time.Hour),
		ExpiresAt:    expires,
	}
}

func obs(amount, conf int64, removed bool) Observation {
	return Observation{TxID: "tx1", AmountSat: amount, Confirmations: conf, Removed: removed}
}

func TestPendingUntilSomethingArrives(t *testing.T) {
	inv := invoice(StatusPending, 100_000, 2, now.Add(time.Hour))
	if got := Decide(inv, nil, now); got != StatusPending {
		t.Fatalf("got %s, want pending", got)
	}
}

func TestMempoolPaymentIsSeen(t *testing.T) {
	inv := invoice(StatusPending, 100_000, 2, now.Add(time.Hour))
	if got := Decide(inv, []Observation{obs(100_000, 0, false)}, now); got != StatusSeen {
		t.Fatalf("got %s, want seen", got)
	}
}

func TestBelowThresholdIsConfirming(t *testing.T) {
	inv := invoice(StatusSeen, 100_000, 3, now.Add(time.Hour))
	if got := Decide(inv, []Observation{obs(100_000, 1, false)}, now); got != StatusConfirming {
		t.Fatalf("got %s, want confirming", got)
	}
}

func TestThresholdReachedAndFullyPaid(t *testing.T) {
	inv := invoice(StatusConfirming, 100_000, 2, now.Add(time.Hour))
	if got := Decide(inv, []Observation{obs(100_000, 2, false)}, now); got != StatusConfirmed {
		t.Fatalf("got %s, want confirmed", got)
	}
}

func TestOverpaymentConfirms(t *testing.T) {
	// Paying more than asked settles the invoice. Refunding the difference is
	// a business decision made elsewhere; it is not a reason to hold the order.
	inv := invoice(StatusConfirming, 100_000, 1, now.Add(time.Hour))
	if got := Decide(inv, []Observation{obs(150_000, 1, false)}, now); got != StatusConfirmed {
		t.Fatalf("got %s, want confirmed", got)
	}
}

func TestUnderpaymentOnlyJudgedAtThreshold(t *testing.T) {
	inv := invoice(StatusSeen, 100_000, 2, now.Add(time.Hour))

	// One confirmation, short of the amount and short of the threshold: the
	// second payment may still be on its way, so this must not be underpaid.
	if got := Decide(inv, []Observation{obs(60_000, 1, false)}, now); got != StatusConfirming {
		t.Fatalf("got %s, want confirming", got)
	}

	// At the threshold and still short — now it is a real underpayment.
	if got := Decide(inv, []Observation{obs(60_000, 2, false)}, now); got != StatusUnderpaid {
		t.Fatalf("got %s, want underpaid", got)
	}
}

func TestTwoPaymentsSumUp(t *testing.T) {
	inv := invoice(StatusSeen, 100_000, 1, now.Add(time.Hour))
	got := Decide(inv, []Observation{
		{TxID: "a", AmountSat: 40_000, Confirmations: 3},
		{TxID: "b", AmountSat: 60_000, Confirmations: 1},
	}, now)
	if got != StatusConfirmed {
		t.Fatalf("got %s, want confirmed", got)
	}
}

func TestWeakestConfirmationGoverns(t *testing.T) {
	// One payment deeply buried, the other just arrived: the invoice is only
	// as settled as its least settled payment.
	inv := invoice(StatusConfirming, 100_000, 2, now.Add(time.Hour))
	got := Decide(inv, []Observation{
		{TxID: "a", AmountSat: 50_000, Confirmations: 10},
		{TxID: "b", AmountSat: 50_000, Confirmations: 0},
	}, now)
	if got != StatusSeen {
		t.Fatalf("got %s, want seen", got)
	}
}

func TestExpiryOnlyAppliesWhileNothingReceived(t *testing.T) {
	expired := now.Add(-time.Minute)

	if got := Decide(invoice(StatusPending, 100_000, 1, expired), nil, now); got != StatusExpired {
		t.Fatalf("got %s, want expired", got)
	}

	// Money is in flight. Expiring now would mean taking a payment and
	// cancelling the order it paid for — never do this.
	got := Decide(invoice(StatusSeen, 100_000, 1, expired), []Observation{obs(100_000, 0, false)}, now)
	if got != StatusSeen {
		t.Fatalf("got %s, want seen — an invoice with money in flight must not expire", got)
	}
}

func TestReorgRemovesTheFunds(t *testing.T) {
	inv := invoice(StatusConfirming, 100_000, 2, now.Add(time.Hour))
	if got := Decide(inv, []Observation{obs(100_000, 1, true)}, now); got != StatusReorged {
		t.Fatalf("got %s, want reorged", got)
	}
}

func TestReorgIsNotTerminal(t *testing.T) {
	// The transaction was dropped and then mined again in the new chain. The
	// invoice has to come back, or a customer who genuinely paid is stuck.
	inv := invoice(StatusReorged, 100_000, 1, now.Add(time.Hour))
	if got := Decide(inv, []Observation{obs(100_000, 1, false)}, now); got != StatusConfirmed {
		t.Fatalf("got %s, want confirmed", got)
	}
}

func TestConfirmedIsSticky(t *testing.T) {
	// Once an order has shipped, a later reorg cannot un-ship it. Whatever is
	// done about it is a manual decision, not an automatic state change.
	inv := invoice(StatusConfirmed, 100_000, 1, now.Add(time.Hour))
	if got := Decide(inv, []Observation{obs(100_000, 1, true)}, now); got != StatusConfirmed {
		t.Fatalf("got %s, want confirmed to stay", got)
	}
}

func TestExpiredIsSticky(t *testing.T) {
	inv := invoice(StatusExpired, 100_000, 1, now.Add(-time.Hour))
	if got := Decide(inv, []Observation{obs(100_000, 5, false)}, now); got != StatusExpired {
		t.Fatalf("got %s, want expired to stay", got)
	}
}

func TestRemovedDoesNotCountTowardTotal(t *testing.T) {
	inv := invoice(StatusConfirming, 100_000, 1, now.Add(time.Hour))
	got := Decide(inv, []Observation{
		{TxID: "a", AmountSat: 50_000, Confirmations: 2},
		{TxID: "b", AmountSat: 50_000, Confirmations: 2, Removed: true},
	}, now)
	if got != StatusUnderpaid {
		t.Fatalf("got %s, want underpaid — a reorged payment is not a payment", got)
	}
}

func TestPaidAndMinConfirmations(t *testing.T) {
	o := []Observation{
		{TxID: "a", AmountSat: 40_000, Confirmations: 5},
		{TxID: "b", AmountSat: 60_000, Confirmations: 2},
		{TxID: "c", AmountSat: 99_000, Confirmations: 9, Removed: true},
	}
	if got := Paid(o); got != 100_000 {
		t.Fatalf("Paid = %d, want 100000", got)
	}
	if got := MinConfirmations(o); got != 2 {
		t.Fatalf("MinConfirmations = %d, want 2", got)
	}
}

func TestMinConfirmationsWithNoLiveObservations(t *testing.T) {
	if got := MinConfirmations([]Observation{{TxID: "a", Confirmations: 4, Removed: true}}); got != 0 {
		t.Fatalf("MinConfirmations = %d, want 0", got)
	}
}
