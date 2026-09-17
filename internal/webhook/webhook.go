// Package webhook delivers status changes to the merchant.
//
// Deliveries come from an outbox table written in the same transaction as the
// status change itself. That ordering is the whole reliability story: a
// webhook cannot be sent for a change that was rolled back, and a change
// cannot be committed without its webhook being queued.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"
)

type Event struct {
	ID            int64  `json:"event_id"`
	InvoiceID     string `json:"invoice_id"`
	Status        string `json:"status"`
	PaidSat       int64  `json:"paid_sat"`
	Confirmations int64  `json:"confirmations"`
	CreatedAt     int64  `json:"created_at"`
}

type Sender struct {
	db       *sql.DB
	url      string
	secret   []byte
	http     *http.Client
	log      *slog.Logger
	interval time.Duration
	maxTries int
	now      func() time.Time
}

type Config struct {
	URL      string
	Secret   string
	Interval time.Duration
	MaxTries int
}

func New(db *sql.DB, cfg Config, log *slog.Logger) *Sender {
	if cfg.Interval == 0 {
		cfg.Interval = 5 * time.Second
	}
	if cfg.MaxTries == 0 {
		cfg.MaxTries = 12 // ~ up to a day with the backoff below
	}
	return &Sender{
		db:       db,
		url:      cfg.URL,
		secret:   []byte(cfg.Secret),
		http:     &http.Client{Timeout: 15 * time.Second},
		log:      log,
		interval: cfg.Interval,
		maxTries: cfg.MaxTries,
		now:      time.Now,
	}
}

func (s *Sender) Run(ctx context.Context) error {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := s.DeliverDue(ctx); err != nil {
				s.log.Error("webhook delivery pass failed", "err", err)
			}
		}
	}
}

// DeliverDue sends every event whose retry time has arrived. Exported so tests
// can drive delivery without waiting on a ticker.
func (s *Sender) DeliverDue(ctx context.Context) error {
	if s.url == "" {
		return nil // no endpoint configured; events accumulate harmlessly
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, invoice_id, status, paid_sat, confirmations, created_at, attempts
		   FROM events
		  WHERE delivered = 0 AND next_try_at <= ?
		  ORDER BY id LIMIT 50`, s.now().Unix())
	if err != nil {
		return err
	}

	type pending struct {
		Event
		attempts int
	}
	var batch []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.ID, &p.InvoiceID, &p.Status, &p.PaidSat,
			&p.Confirmations, &p.CreatedAt, &p.attempts); err != nil {
			rows.Close()
			return err
		}
		batch = append(batch, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, p := range batch {
		err := s.post(ctx, p.Event)
		if err == nil {
			if _, err := s.db.ExecContext(ctx,
				`UPDATE events SET delivered = 1, attempts = attempts + 1, last_error = '' WHERE id = ?`,
				p.ID); err != nil {
				return err
			}
			s.log.Info("webhook delivered", "event", p.ID, "invoice", p.InvoiceID, "status", p.Status)
			continue
		}

		attempts := p.attempts + 1
		if attempts >= s.maxTries {
			// Give up, but keep the row and the reason. A dropped webhook that
			// leaves no trace is indistinguishable from one that was never
			// generated, and the merchant will ask.
			if _, dbErr := s.db.ExecContext(ctx,
				`UPDATE events SET attempts = ?, last_error = ?, next_try_at = ? WHERE id = ?`,
				attempts, "giving up: "+err.Error(), math.MaxInt32, p.ID); dbErr != nil {
				return dbErr
			}
			s.log.Error("webhook gave up", "event", p.ID, "attempts", attempts, "err", err)
			continue
		}

		next := s.now().Add(backoff(attempts)).Unix()
		if _, dbErr := s.db.ExecContext(ctx,
			`UPDATE events SET attempts = ?, last_error = ?, next_try_at = ? WHERE id = ?`,
			attempts, err.Error(), next, p.ID); dbErr != nil {
			return dbErr
		}
		s.log.Warn("webhook failed, will retry", "event", p.ID, "attempt", attempts, "err", err)
	}
	return nil
}

// backoff grows exponentially and stops at an hour: 10s, 20s, 40s … 1h.
// Capping matters — an uncapped doubling reaches "next week" after a dozen
// failures, and the merchant who fixed their endpoint waits a week for the
// retry.
func backoff(attempt int) time.Duration {
	d := time.Duration(10*math.Pow(2, float64(attempt-1))) * time.Second
	if d > time.Hour {
		return time.Hour
	}
	return d
}

func (s *Sender) post(ctx context.Context, ev Event) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	ts := strconv.FormatInt(s.now().Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Webhook-Timestamp", ts)
	req.Header.Set("X-Webhook-Signature", Sign(s.secret, ts, body))
	// The event id is stable across retries, so the receiver can deduplicate.
	// At-least-once delivery is the only thing an HTTP callback can promise;
	// making the id explicit is what turns that into something usable.
	req.Header.Set("X-Idempotency-Key", strconv.FormatInt(ev.ID, 10))

	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("endpoint returned %d", resp.StatusCode)
	}
	return nil
}

// Sign returns the hex HMAC-SHA256 over "timestamp.body".
//
// The timestamp is inside the signed material, not merely alongside it: sign
// only the body and a captured request can be replayed forever, because the
// signature stays valid.
func Sign(secret []byte, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify is what a receiver should use. Constant-time comparison, and the
// timestamp must be recent.
func Verify(secret []byte, timestamp string, body []byte, signature string, now time.Time, tolerance time.Duration) bool {
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	age := now.Sub(time.Unix(ts, 0))
	if age < -tolerance || age > tolerance {
		return false
	}
	expected := Sign(secret, timestamp, body)
	return hmac.Equal([]byte(expected), []byte(signature))
}
