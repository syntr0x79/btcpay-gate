package health

import (
	"errors"
	"testing"
	"time"

	"github.com/syntr0x79/btcpay-gate/internal/rpc"
)

func at(base time.Time, d time.Duration) func() time.Time {
	return func() time.Time { return base.Add(d) }
}

// Before the first poll completes there is no evidence the gateway can read
// the chain at all. Reporting healthy here is the exact failure this package
// exists to prevent: on 2026-09-22 the gateway answered `ok` for six hours
// while every poll failed.
func TestNotHealthyBeforeFirstPoll(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	s := New(5 * time.Minute)
	s.now = at(base, 0)

	got := s.Report()
	if got.OK {
		t.Fatalf("healthy before any poll: %+v", got)
	}
	if got.Reason == "" {
		t.Error("no reason given for an unhealthy report")
	}
}

func TestHealthyAfterSuccessfulPoll(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	s := New(5 * time.Minute)
	s.now = at(base, 0)
	s.PollSucceeded()

	got := s.Report()
	if !got.OK {
		t.Fatalf("unhealthy after a successful poll: %+v", got)
	}
	if !got.WalletLoaded {
		t.Error("WalletLoaded is false after a successful poll")
	}
	if !got.LastSuccess.Equal(base) {
		t.Errorf("LastSuccess = %v, want %v", got.LastSuccess, base)
	}
}

// A poll that fails because the node is busy says nothing about the wallet.
// During initial block download bitcoind regularly misses the RPC timeout;
// restarting the gateway for that would trade a slow node for a flapping one.
func TestTransientFailureStaysHealthyUntilMaxAge(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	s := New(5 * time.Minute)
	s.now = at(base, 0)
	s.PollSucceeded()

	s.now = at(base, 30*time.Second)
	s.PollFailed(errors.New("context deadline exceeded"))

	got := s.Report()
	if !got.OK {
		t.Fatalf("one timeout 30s after a good poll made the gateway unhealthy: %+v", got)
	}
	if !got.WalletLoaded {
		t.Error("a timeout cleared WalletLoaded; a timeout is not evidence about the wallet")
	}
}

func TestStalePollsGoUnhealthy(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	s := New(5 * time.Minute)
	s.now = at(base, 0)
	s.PollSucceeded()

	s.now = at(base, 5*time.Minute+time.Second)
	got := s.Report()
	if got.OK {
		t.Fatalf("healthy with the last good poll older than maxAge: %+v", got)
	}
}

// -18 is unambiguous: the node has no wallet by that name loaded, so every
// subsequent poll will fail the same way. Waiting out maxAge before saying so
// adds blindness for no gain.
func TestWalletNotLoadedIsImmediatelyUnhealthy(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	s := New(5 * time.Minute)
	s.now = at(base, 0)
	s.PollSucceeded()

	s.now = at(base, time.Second)
	s.PollFailed(&rpc.Error{Code: -18, Message: "Requested wallet does not exist or is not loaded"})

	got := s.Report()
	if got.OK {
		t.Fatalf("healthy while the wallet is not loaded: %+v", got)
	}
	if got.WalletLoaded {
		t.Error("WalletLoaded stayed true after -18")
	}
}

func TestRecoversAfterWalletComesBack(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	s := New(5 * time.Minute)
	s.now = at(base, 0)
	s.PollFailed(&rpc.Error{Code: -18, Message: "Requested wallet does not exist or is not loaded"})

	s.now = at(base, time.Second)
	s.PollSucceeded()

	if got := s.Report(); !got.OK {
		t.Fatalf("stayed unhealthy after the wallet came back: %+v", got)
	}
}
