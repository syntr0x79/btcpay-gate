// Package health holds the one fact the gateway's readiness depends on:
// whether the chain monitor is currently able to read the wallet.
//
// It exists because the honest answer to "is this service working?" is not
// "is the HTTP listener up?". On 2026-09-22 those two answers diverged for six
// hours: bitcoind restarted, the wallet stayed unloaded, every poll failed
// with -18, and /healthz — a constant 200 — reported success throughout. The
// outage was eventually noticed by the backup system, which is not a monitor.
//
// The state is written by the monitor, which is the only component that finds
// out, and read by the HTTP layer, which is the only component anyone asks.
package health

import (
	"fmt"
	"sync"
	"time"

	"github.com/syntr0x79/btcpay-gate/internal/rpc"
)

// DefaultMaxPollAge is how stale the last successful poll may get before the
// gateway calls itself unhealthy.
//
// Generous on purpose. With a ten-second poll interval this tolerates thirty
// consecutive failures, and it has to: during initial block download bitcoind
// routinely misses the RPC timeout, and a threshold tight enough to catch that
// would have docker restarting the gateway every few minutes. The failure this
// guards against — a poll loop that is dead rather than slow — does not
// recover on its own, so waiting five minutes to declare it costs nothing.
const DefaultMaxPollAge = 5 * time.Minute

type Report struct {
	OK bool
	// Reason is empty when OK, and human-readable otherwise: it ends up in the
	// /healthz body and therefore in `docker inspect`, which is where someone
	// debugging at 3am actually looks.
	Reason       string
	WalletLoaded bool
	// LastSuccess is the zero time until the first poll succeeds.
	LastSuccess time.Time
}

type State struct {
	maxAge time.Duration

	mu           sync.Mutex
	now          func() time.Time
	lastSuccess  time.Time
	walletLoaded bool
}

func New(maxAge time.Duration) *State {
	if maxAge <= 0 {
		maxAge = DefaultMaxPollAge
	}
	return &State{
		maxAge: maxAge,
		now:    time.Now,
		// True until proven otherwise: the process only reaches the monitor
		// after wallet.Open has loaded the wallet, so starting at false would
		// report a problem that has already been ruled out.
		walletLoaded: true,
	}
}

func (s *State) PollSucceeded() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastSuccess = s.now()
	s.walletLoaded = true
}

// PollFailed records one failed poll, and classifies it.
//
// Only -18 clears walletLoaded. A timeout, a connection refused, a 500 — none
// of those are evidence about the wallet, and demoting the wallet on every
// transient error would make the distinction useless exactly when it matters.
func (s *State) PollFailed(err error) {
	if !rpc.IsWalletNotLoaded(err) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.walletLoaded = false
}

func (s *State) Report() Report {
	s.mu.Lock()
	defer s.mu.Unlock()

	r := Report{WalletLoaded: s.walletLoaded, LastSuccess: s.lastSuccess}
	switch {
	case !s.walletLoaded:
		// Immediate, without waiting out maxAge: -18 will not fix itself, and
		// every second spent looking healthy is a second a payment can arrive
		// unseen.
		r.Reason = "bitcoind has no wallet loaded for this gateway"
	case s.lastSuccess.IsZero():
		r.Reason = "no chain poll has succeeded yet"
	case s.now().Sub(s.lastSuccess) > s.maxAge:
		r.Reason = fmt.Sprintf("last successful chain poll was %s ago, over the %s limit",
			s.now().Sub(s.lastSuccess).Truncate(time.Second), s.maxAge)
	default:
		r.OK = true
	}
	return r
}
