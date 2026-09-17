package webhook

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/syntr0x79/btcpay-gate/internal/payments"
)

func newDB(t *testing.T) *sql.DB {
	t.Helper()
	st, err := payments.Open(filepath.Join(t.TempDir(), "hooks.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st.DB()
}

func queueEvent(t *testing.T, db *sql.DB, invoiceID, status string) int64 {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO events (invoice_id, status, paid_sat, confirmations, created_at, next_try_at)
		 VALUES (?, ?, ?, ?, ?, 0)`, invoiceID, status, 100_000, 2, time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func eventRow(t *testing.T, db *sql.DB, id int64) (delivered, attempts int, lastErr string, nextTry int64) {
	t.Helper()
	err := db.QueryRow(`SELECT delivered, attempts, last_error, next_try_at FROM events WHERE id = ?`, id).
		Scan(&delivered, &attempts, &lastErr, &nextTry)
	if err != nil {
		t.Fatal(err)
	}
	return
}

func TestDeliversAndMarksDelivered(t *testing.T) {
	db := newDB(t)
	id := queueEvent(t, db, "inv-1", "confirmed")

	var got atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := New(db, Config{URL: srv.URL, Secret: "topsecret"}, quietLog())
	if err := s.DeliverDue(context.Background()); err != nil {
		t.Fatal(err)
	}

	if got.Load() != 1 {
		t.Fatalf("endpoint hit %d times, want 1", got.Load())
	}
	delivered, attempts, _, _ := eventRow(t, db, id)
	if delivered != 1 || attempts != 1 {
		t.Fatalf("delivered=%d attempts=%d, want 1/1", delivered, attempts)
	}
}

func TestDeliveredEventIsNotSentAgain(t *testing.T) {
	db := newDB(t)
	queueEvent(t, db, "inv-1", "confirmed")

	var got atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Add(1)
	}))
	defer srv.Close()

	s := New(db, Config{URL: srv.URL, Secret: "s"}, quietLog())
	for i := 0; i < 3; i++ {
		if err := s.DeliverDue(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got.Load() != 1 {
		t.Fatalf("endpoint hit %d times, want 1", got.Load())
	}
}

func TestFailureSchedulesARetryWithBackoff(t *testing.T) {
	db := newDB(t)
	id := queueEvent(t, db, "inv-1", "confirmed")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	base := time.Now()
	s := New(db, Config{URL: srv.URL, Secret: "s"}, quietLog())
	s.now = func() time.Time { return base }

	if err := s.DeliverDue(context.Background()); err != nil {
		t.Fatal(err)
	}

	delivered, attempts, lastErr, nextTry := eventRow(t, db, id)
	if delivered != 0 {
		t.Fatal("a 500 must not count as delivered")
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	if lastErr == "" {
		t.Fatal("the failure reason must be recorded")
	}
	if want := base.Add(10 * time.Second).Unix(); nextTry != want {
		t.Fatalf("next_try_at = %d, want %d (first backoff is 10s)", nextTry, want)
	}
}

func TestRetryIsNotAttemptedBeforeItsTime(t *testing.T) {
	db := newDB(t)
	queueEvent(t, db, "inv-1", "confirmed")

	var got atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	base := time.Now()
	s := New(db, Config{URL: srv.URL, Secret: "s"}, quietLog())
	s.now = func() time.Time { return base }

	// First attempt fails and schedules +10s.
	if err := s.DeliverDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A pass five seconds later must not touch it.
	s.now = func() time.Time { return base.Add(5 * time.Second) }
	if err := s.DeliverDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got.Load() != 1 {
		t.Fatalf("endpoint hit %d times, want 1 — the backoff was ignored", got.Load())
	}

	// Once the backoff has elapsed it is tried again.
	s.now = func() time.Time { return base.Add(11 * time.Second) }
	if err := s.DeliverDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got.Load() != 2 {
		t.Fatalf("endpoint hit %d times, want 2", got.Load())
	}
}

func TestGivesUpAfterMaxTriesButKeepsTheRecord(t *testing.T) {
	db := newDB(t)
	id := queueEvent(t, db, "inv-1", "confirmed")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	base := time.Now()
	s := New(db, Config{URL: srv.URL, Secret: "s", MaxTries: 3}, quietLog())

	for i := 0; i < 3; i++ {
		s.now = func() time.Time { return base.Add(time.Duration(i) * time.Hour) }
		if err := s.DeliverDue(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	delivered, attempts, lastErr, _ := eventRow(t, db, id)
	if delivered != 0 {
		t.Fatal("never delivered, must not be marked delivered")
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
	if lastErr == "" || lastErr[:11] != "giving up: " {
		t.Fatalf("last_error = %q, want it to record giving up", lastErr)
	}
}

func TestBackoffIsCapped(t *testing.T) {
	if got := backoff(1); got != 10*time.Second {
		t.Fatalf("backoff(1) = %s, want 10s", got)
	}
	if got := backoff(2); got != 20*time.Second {
		t.Fatalf("backoff(2) = %s, want 20s", got)
	}
	// Without a cap, a dozen failures would push the next attempt a week out,
	// and a merchant who has just fixed their endpoint would wait a week.
	if got := backoff(30); got != time.Hour {
		t.Fatalf("backoff(30) = %s, want it capped at 1h", got)
	}
}

func TestSignatureAndIdempotencyHeaders(t *testing.T) {
	db := newDB(t)
	id := queueEvent(t, db, "inv-1", "confirmed")

	secret := "topsecret"
	type captured struct {
		body      []byte
		signature string
		timestamp string
		idemKey   string
	}
	var c captured

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.body, _ = io.ReadAll(r.Body)
		c.signature = r.Header.Get("X-Webhook-Signature")
		c.timestamp = r.Header.Get("X-Webhook-Timestamp")
		c.idemKey = r.Header.Get("X-Idempotency-Key")
	}))
	defer srv.Close()

	s := New(db, Config{URL: srv.URL, Secret: secret}, quietLog())
	if err := s.DeliverDue(context.Background()); err != nil {
		t.Fatal(err)
	}

	if !Verify([]byte(secret), c.timestamp, c.body, c.signature, time.Now(), 5*time.Minute) {
		t.Fatal("receiver could not verify the signature we sent")
	}
	if c.idemKey == "" {
		t.Fatal("idempotency key missing — at-least-once delivery is unusable without it")
	}
	if want := "1"; c.idemKey != want {
		t.Fatalf("idempotency key = %q, want the event id %q", c.idemKey, want)
	}
	_ = id
}

func TestVerifyRejectsTampering(t *testing.T) {
	secret := []byte("s")
	body := []byte(`{"invoice_id":"inv-1","status":"confirmed"}`)
	now := time.Now()
	ts := "1789640000"
	sig := Sign(secret, ts, body)

	if !Verify(secret, ts, body, sig, time.Unix(1789640000, 0), time.Minute) {
		t.Fatal("a genuine signature must verify")
	}

	tampered := []byte(`{"invoice_id":"inv-1","status":"confirmed","paid_sat":999}`)
	if Verify(secret, ts, tampered, sig, time.Unix(1789640000, 0), time.Minute) {
		t.Fatal("a modified body must not verify")
	}
	if Verify([]byte("wrong"), ts, body, sig, time.Unix(1789640000, 0), time.Minute) {
		t.Fatal("a wrong secret must not verify")
	}
	_ = now
}

func TestVerifyRejectsAReplayedRequest(t *testing.T) {
	// The timestamp is inside the signed material, so an old capture fails on
	// age rather than on signature — which is the point: sign the body alone
	// and a captured request stays valid forever.
	secret := []byte("s")
	body := []byte(`{"status":"confirmed"}`)
	ts := "1789640000"
	sig := Sign(secret, ts, body)

	old := time.Unix(1789640000, 0).Add(2 * time.Hour)
	if Verify(secret, ts, body, sig, old, 5*time.Minute) {
		t.Fatal("a request older than the tolerance must be rejected")
	}
}

func TestNoEndpointConfiguredIsNotAnError(t *testing.T) {
	db := newDB(t)
	queueEvent(t, db, "inv-1", "confirmed")

	s := New(db, Config{}, quietLog())
	if err := s.DeliverDue(context.Background()); err != nil {
		t.Fatalf("running without a webhook URL must be harmless, got %v", err)
	}
}
