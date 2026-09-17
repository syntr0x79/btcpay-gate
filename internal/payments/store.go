package payments

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrNotFound = errors.New("invoice not found")

// Store is the persistence boundary. Everything here is written so a crash at
// any point is survivable: the chain cursor and the invoice state advance in
// one transaction, so the service can never have acknowledged a block whose
// payments it failed to record.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS invoices (
    id            TEXT PRIMARY KEY,
    address       TEXT NOT NULL UNIQUE,
    deriv_index   INTEGER NOT NULL,
    amount_sat    INTEGER NOT NULL,
    required_conf INTEGER NOT NULL,
    status        TEXT NOT NULL,
    created_at    INTEGER NOT NULL,
    expires_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS invoices_status ON invoices(status);

CREATE TABLE IF NOT EXISTS observations (
    invoice_id    TEXT NOT NULL REFERENCES invoices(id),
    txid          TEXT NOT NULL,
    vout          INTEGER NOT NULL,
    amount_sat    INTEGER NOT NULL,
    confirmations INTEGER NOT NULL,
    removed       INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (txid, vout)
);
CREATE INDEX IF NOT EXISTS observations_invoice ON observations(invoice_id);

-- Single-row table holding the chain cursor.
CREATE TABLE IF NOT EXISTS cursor (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    block_hash TEXT NOT NULL
);

-- Outbox: a status change becomes a durable row in the same transaction that
-- records the change. Delivering from here means a webhook cannot be lost by a
-- crash, and cannot be sent for a change that was rolled back.
CREATE TABLE IF NOT EXISTS events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    invoice_id  TEXT NOT NULL,
    status      TEXT NOT NULL,
    paid_sat    INTEGER NOT NULL,
    confirmations INTEGER NOT NULL,
    created_at  INTEGER NOT NULL,
    delivered   INTEGER NOT NULL DEFAULT 0,
    attempts    INTEGER NOT NULL DEFAULT 0,
    next_try_at INTEGER NOT NULL DEFAULT 0,
    last_error  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS events_undelivered ON events(delivered, next_try_at);
`

func Open(dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One writer. SQLite allows exactly one anyway; making it explicit avoids
	// "database is locked" surprises under concurrent HTTP requests.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) CreateInvoice(ctx context.Context, inv Invoice) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO invoices (id, address, deriv_index, amount_sat, required_conf, status, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		inv.ID, inv.Address, inv.DerivIndex, inv.AmountSat, inv.RequiredConf,
		string(inv.Status), inv.CreatedAt.Unix(), inv.ExpiresAt.Unix())
	return err
}

func scanInvoice(row interface{ Scan(...any) error }) (Invoice, error) {
	var (
		inv                  Invoice
		status               string
		createdAt, expiresAt int64
	)
	err := row.Scan(&inv.ID, &inv.Address, &inv.DerivIndex, &inv.AmountSat,
		&inv.RequiredConf, &status, &createdAt, &expiresAt)
	if err != nil {
		return Invoice{}, err
	}
	inv.Status = Status(status)
	inv.CreatedAt = time.Unix(createdAt, 0).UTC()
	inv.ExpiresAt = time.Unix(expiresAt, 0).UTC()
	return inv, nil
}

const invoiceColumns = `id, address, deriv_index, amount_sat, required_conf, status, created_at, expires_at`

func (s *Store) Invoice(ctx context.Context, id string) (Invoice, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+invoiceColumns+` FROM invoices WHERE id = ?`, id)
	inv, err := scanInvoice(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Invoice{}, ErrNotFound
	}
	return inv, err
}

func (s *Store) InvoiceByAddress(ctx context.Context, address string) (Invoice, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+invoiceColumns+` FROM invoices WHERE address = ?`, address)
	inv, err := scanInvoice(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Invoice{}, ErrNotFound
	}
	return inv, err
}

// OpenInvoices returns everything still worth recomputing.
func (s *Store) OpenInvoices(ctx context.Context) ([]Invoice, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+invoiceColumns+` FROM invoices WHERE status NOT IN (?, ?)`,
		string(StatusConfirmed), string(StatusExpired))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Invoice
	for rows.Next() {
		inv, err := scanInvoice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

func (s *Store) Observations(ctx context.Context, invoiceID string) ([]Observation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT txid, vout, amount_sat, confirmations, removed FROM observations WHERE invoice_id = ?`,
		invoiceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Observation
	for rows.Next() {
		var o Observation
		var removed int
		if err := rows.Scan(&o.TxID, &o.Vout, &o.AmountSat, &o.Confirmations, &removed); err != nil {
			return nil, err
		}
		o.Removed = removed != 0
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	SortObservations(out)
	return out, nil
}

func (s *Store) Cursor(ctx context.Context) (string, error) {
	var hash string
	err := s.db.QueryRowContext(ctx, `SELECT block_hash FROM cursor WHERE id = 1`).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return hash, err
}

// Update is the only write path used by the monitor. Observations, invoice
// statuses, emitted events and the chain cursor all move in one transaction:
// either the service has processed a block completely, or it has not processed
// it at all and will see it again on the next pass.
type Update struct {
	Observations []ObservationUpdate
	Cursor       string
}

type ObservationUpdate struct {
	InvoiceID string
	Observation
}

// StatusChange is what the recompute produced; returned so the caller can log
// it. Events are already persisted by the time this returns.
type StatusChange struct {
	InvoiceID string
	From, To  Status
}

func (s *Store) Apply(ctx context.Context, u Update, now time.Time) ([]StatusChange, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	for _, o := range u.Observations {
		removed := 0
		if o.Removed {
			removed = 1
		}
		// An observation can arrive many times — every poll re-reports the
		// same transaction with a higher confirmation count. Upsert keyed on
		// (txid, vout) makes reprocessing a block harmless, which is what lets
		// the cursor be replayed after a crash.
		_, err := tx.ExecContext(ctx,
			`INSERT INTO observations (invoice_id, txid, vout, amount_sat, confirmations, removed)
			 VALUES (?, ?, ?, ?, ?, ?)
			 ON CONFLICT(txid, vout) DO UPDATE SET
			   confirmations = excluded.confirmations,
			   removed       = excluded.removed,
			   amount_sat    = excluded.amount_sat`,
			o.InvoiceID, o.TxID, o.Vout, o.AmountSat, o.Confirmations, removed)
		if err != nil {
			return nil, fmt.Errorf("upsert observation %s:%d: %w", o.TxID, o.Vout, err)
		}
	}

	// Expiry has to be evaluated even for invoices nothing arrived for.
	openRows, err := tx.QueryContext(ctx,
		`SELECT `+invoiceColumns+` FROM invoices WHERE status NOT IN (?, ?)`,
		string(StatusConfirmed), string(StatusExpired))
	if err != nil {
		return nil, err
	}
	var open []Invoice
	for openRows.Next() {
		inv, err := scanInvoice(openRows)
		if err != nil {
			openRows.Close()
			return nil, err
		}
		open = append(open, inv)
	}
	openRows.Close()
	if err := openRows.Err(); err != nil {
		return nil, err
	}

	var changes []StatusChange
	for _, inv := range open {
		obs, err := observationsTx(ctx, tx, inv.ID)
		if err != nil {
			return nil, err
		}
		next := Decide(inv, obs, now)
		if next == inv.Status {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE invoices SET status = ? WHERE id = ?`,
			string(next), inv.ID); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO events (invoice_id, status, paid_sat, confirmations, created_at, next_try_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			inv.ID, string(next), Paid(obs), MinConfirmations(obs), now.Unix(), now.Unix()); err != nil {
			return nil, err
		}
		changes = append(changes, StatusChange{InvoiceID: inv.ID, From: inv.Status, To: next})
	}

	if u.Cursor != "" {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO cursor (id, block_hash) VALUES (1, ?)
			 ON CONFLICT(id) DO UPDATE SET block_hash = excluded.block_hash`, u.Cursor); err != nil {
			return nil, err
		}
	}

	return changes, tx.Commit()
}

func observationsTx(ctx context.Context, tx *sql.Tx, invoiceID string) ([]Observation, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT txid, vout, amount_sat, confirmations, removed FROM observations WHERE invoice_id = ?`,
		invoiceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Observation
	for rows.Next() {
		var o Observation
		var removed int
		if err := rows.Scan(&o.TxID, &o.Vout, &o.AmountSat, &o.Confirmations, &removed); err != nil {
			return nil, err
		}
		o.Removed = removed != 0
		out = append(out, o)
	}
	return out, rows.Err()
}

// NextDerivIndex returns the next unused derivation index. Addresses are never
// reused: a reused address makes two invoices indistinguishable on-chain, and
// then a payment for one settles the other.
func (s *Store) NextDerivIndex(ctx context.Context) (int, error) {
	var idx sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MAX(deriv_index) FROM invoices`).Scan(&idx)
	if err != nil {
		return 0, err
	}
	if !idx.Valid {
		return 0, nil
	}
	return int(idx.Int64) + 1, nil
}

// DB exposes the underlying handle so the webhook sender can read the outbox
// written by Apply. Same database by design: the event and the status change
// it describes must commit together or not at all.
func (s *Store) DB() *sql.DB { return s.db }
