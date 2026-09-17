// Package payments holds the invoice lifecycle.
//
// The transition rule is a pure function of (invoice, observations, now). It
// touches no database, no clock and no node, which is what makes the awkward
// cases — reorgs, underpayment, a payment arriving after expiry — testable
// without a blockchain.
package payments

import (
	"sort"
	"time"
)

type Status string

const (
	// Nothing seen for this address yet.
	StatusPending Status = "pending"
	// At least one payment in the mempool, no confirmations.
	StatusSeen Status = "seen"
	// Confirmed at least once, still below the required threshold.
	StatusConfirming Status = "confirming"
	// Fully paid and at or above the threshold. Terminal in the happy path.
	StatusConfirmed Status = "confirmed"
	// Enough confirmations, but the total received is short of the amount.
	StatusUnderpaid Status = "underpaid"
	// Expiry passed with nothing received.
	StatusExpired Status = "expired"
	// Funds that had been counted are no longer in the chain. Not terminal:
	// the transaction may be mined again, so the invoice goes back through the
	// normal path if it reappears.
	StatusReorged Status = "reorged"
)

// Terminal reports whether a status should stop being recomputed. Only
// confirmed and expired are final — reorged deliberately is not, because a
// transaction dropped by one reorg can be mined by the next block.
func (s Status) Terminal() bool {
	return s == StatusConfirmed || s == StatusExpired
}

type Invoice struct {
	ID           string
	Address      string
	DerivIndex   int
	AmountSat    int64
	RequiredConf int64
	Status       Status
	CreatedAt    time.Time
	ExpiresAt    time.Time
}

// Observation is one payment seen against an invoice address.
type Observation struct {
	TxID          string
	Vout          int64
	AmountSat     int64
	Confirmations int64
	// Removed means the transaction left the chain: either core reported it in
	// listsinceblock's removed[], or a conflicting transaction was mined.
	Removed bool
}

// Decide returns the status the invoice should have, given what has been seen.
//
// Ordering matters and is the substance of the rule:
//
//  1. Removed observations never count toward the total. A reorged payment is
//     not a payment.
//  2. Expiry only applies while nothing has been received. Once money is in
//     flight, the clock stops — expiring an invoice that someone has already
//     paid is the worst outcome available to this system.
//  3. Underpayment is judged only at the confirmation threshold. Below it the
//     total can still grow, and calling an invoice underpaid while the second
//     transaction is still in the mempool produces a false alarm.
func Decide(inv Invoice, obs []Observation, now time.Time) Status {
	if inv.Status.Terminal() {
		return inv.Status
	}

	var (
		live       []Observation
		removedAny bool
	)
	for _, o := range obs {
		if o.Removed {
			removedAny = true
			continue
		}
		live = append(live, o)
	}

	if len(live) == 0 {
		// Something was counted before and is now gone.
		if removedAny && inv.Status != StatusPending && inv.Status != StatusExpired {
			return StatusReorged
		}
		if !now.Before(inv.ExpiresAt) {
			return StatusExpired
		}
		return StatusPending
	}

	var total int64
	minConf := int64(1<<62 - 1)
	for _, o := range live {
		total += o.AmountSat
		if o.Confirmations < minConf {
			minConf = o.Confirmations
		}
	}

	if minConf <= 0 {
		return StatusSeen
	}
	if minConf < inv.RequiredConf {
		return StatusConfirming
	}
	if total < inv.AmountSat {
		return StatusUnderpaid
	}
	return StatusConfirmed
}

// Paid sums the observations that currently count.
func Paid(obs []Observation) int64 {
	var total int64
	for _, o := range obs {
		if !o.Removed {
			total += o.AmountSat
		}
	}
	return total
}

// MinConfirmations reports the weakest confirmation count among live
// observations; 0 when there are none. The weakest one governs, because an
// invoice is only as settled as its least settled payment.
func MinConfirmations(obs []Observation) int64 {
	min := int64(0)
	first := true
	for _, o := range obs {
		if o.Removed {
			continue
		}
		if first || o.Confirmations < min {
			min, first = o.Confirmations, false
		}
	}
	return min
}

// SortObservations gives deterministic ordering for storage and for tests.
func SortObservations(obs []Observation) {
	sort.Slice(obs, func(i, j int) bool {
		if obs[i].TxID != obs[j].TxID {
			return obs[i].TxID < obs[j].TxID
		}
		return obs[i].Vout < obs[j].Vout
	})
}
