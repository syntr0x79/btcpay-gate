// Package api is the merchant-facing HTTP surface.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/syntr0x79/btcpay-gate/internal/health"
	"github.com/syntr0x79/btcpay-gate/internal/payments"
)

// HealthSource is the chain monitor's view of itself. Kept as an interface so
// the HTTP layer can be tested without a node or a poll loop.
type HealthSource interface {
	Report() health.Report
}

// AddressSource hands out a fresh receiving address. Kept as an interface so
// the HTTP layer can be tested without a node.
type AddressSource interface {
	AddressAt(ctx context.Context, index int) (string, error)
}

type Server struct {
	store       *payments.Store
	addresses   AddressSource
	defaultConf int64
	defaultTTL  time.Duration
	health      HealthSource
	log         *slog.Logger
	now         func() time.Time
}

type Config struct {
	DefaultConfirmations int64
	DefaultTTL           time.Duration
	// Health is what /healthz and /metrics answer from. Leaving it nil is
	// treated as a wiring mistake and reported as unhealthy rather than
	// defaulted to green: an endpoint that cannot be wrong is the bug this
	// field was added to fix.
	Health HealthSource
}

func New(store *payments.Store, addresses AddressSource, cfg Config, log *slog.Logger) *Server {
	if cfg.DefaultConfirmations == 0 {
		cfg.DefaultConfirmations = 2
	}
	if cfg.DefaultTTL == 0 {
		cfg.DefaultTTL = 30 * time.Minute
	}
	return &Server{
		store:       store,
		addresses:   addresses,
		defaultConf: cfg.DefaultConfirmations,
		defaultTTL:  cfg.DefaultTTL,
		health:      cfg.Health,
		log:         log,
		now:         time.Now,
	}
}

func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /invoices", s.createInvoice)
	mux.HandleFunc("GET /invoices/{id}", s.getInvoice)
	mux.HandleFunc("GET /healthz", s.healthz)
	// Also on the API listener, which nothing publishes: convenient when
	// debugging from inside the container, and harmless because reaching this
	// mux at all already means being inside the compose network.
	mux.HandleFunc("GET /metrics", s.metrics)
	return mux
}

// MetricsRoutes is the mux for the dedicated metrics listener.
//
// Separate from Routes, and carrying nothing else, because this is the one
// listener published to the monitoring mesh, and it is published without any
// authentication — whatever reaches it, reads it. Putting POST /invoices on
// the same mux would let every machine in the mesh issue invoices.
func (s *Server) MetricsRoutes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", s.metrics)
	return mux
}

// report is the health state, or a refusal to guess when nothing was wired in.
func (s *Server) report() health.Report {
	if s.health == nil {
		return health.Report{Reason: "health state not wired into the HTTP layer"}
	}
	return s.health.Report()
}

// healthz answers the question the service exists to answer — can it see
// payments? — rather than "is this process listening?".
//
// 503 and not 500: this is the code docker's healthcheck and any load balancer
// already treat as "take it out of rotation", and taking a gateway that cannot
// read the chain out of rotation is exactly right. The reason travels in the
// body so it reaches `docker inspect` and the logs of whatever probed it.
func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	r := s.report()
	if !r.OK {
		http.Error(w, r.Reason, http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok\n"))
}

// metrics is a hand-written prometheus exposition of the two numbers worth
// alerting on.
//
// Hand-written for the same reason the RPC client is: pulling client_golang in
// to print four lines would add more dependency surface to a service that
// touches money than the code it replaces.
//
// Both gauges are always emitted, including as zero. An alert written as
// `paygate_wallet_loaded == 0` matches nothing if the series simply vanishes,
// which would reproduce the original failure in a new place.
func (s *Server) metrics(w http.ResponseWriter, _ *http.Request) {
	r := s.report()

	loaded := 0
	if r.WalletLoaded {
		loaded = 1
	}
	var lastPoll int64
	if !r.LastSuccess.IsZero() {
		lastPoll = r.LastSuccess.Unix()
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintf(w, `# HELP paygate_wallet_loaded Whether bitcoind currently has this gateway's wallet loaded.
# TYPE paygate_wallet_loaded gauge
paygate_wallet_loaded %d
# HELP paygate_last_poll_timestamp_seconds Unix time of the last chain poll that succeeded, 0 if none has.
# TYPE paygate_last_poll_timestamp_seconds gauge
paygate_last_poll_timestamp_seconds %d
`, loaded, lastPoll)
}

type createRequest struct {
	AmountSat    int64  `json:"amount_sat"`
	RequiredConf *int64 `json:"required_conf,omitempty"`
	TTLSeconds   *int64 `json:"ttl_seconds,omitempty"`
}

type invoiceResponse struct {
	ID            string `json:"id"`
	Address       string `json:"address"`
	AmountSat     int64  `json:"amount_sat"`
	PaidSat       int64  `json:"paid_sat"`
	RequiredConf  int64  `json:"required_conf"`
	Confirmations int64  `json:"confirmations"`
	Status        string `json:"status"`
	CreatedAt     string `json:"created_at"`
	ExpiresAt     string `json:"expires_at"`
}

func (s *Server) createInvoice(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed JSON body")
		return
	}
	if req.AmountSat <= 0 {
		writeError(w, http.StatusBadRequest, "amount_sat must be a positive number of satoshi")
		return
	}

	conf := s.defaultConf
	if req.RequiredConf != nil {
		if *req.RequiredConf < 0 {
			writeError(w, http.StatusBadRequest, "required_conf cannot be negative")
			return
		}
		conf = *req.RequiredConf
	}
	ttl := s.defaultTTL
	if req.TTLSeconds != nil {
		if *req.TTLSeconds <= 0 {
			writeError(w, http.StatusBadRequest, "ttl_seconds must be positive")
			return
		}
		ttl = time.Duration(*req.TTLSeconds) * time.Second
	}

	ctx := r.Context()
	index, err := s.store.NextDerivIndex(ctx)
	if err != nil {
		s.fail(w, "allocate derivation index", err)
		return
	}
	// Each invoice gets its own address. Reusing one would make two invoices
	// indistinguishable on-chain, and a payment for one would settle the other.
	address, err := s.addresses.AddressAt(ctx, index)
	if err != nil {
		s.fail(w, "derive address", err)
		return
	}

	now := s.now().UTC()
	inv := payments.Invoice{
		ID:           newID(),
		Address:      address,
		DerivIndex:   index,
		AmountSat:    req.AmountSat,
		RequiredConf: conf,
		Status:       payments.StatusPending,
		CreatedAt:    now,
		ExpiresAt:    now.Add(ttl),
	}
	if err := s.store.CreateInvoice(ctx, inv); err != nil {
		s.fail(w, "store invoice", err)
		return
	}

	writeJSON(w, http.StatusCreated, invoiceResponse{
		ID:           inv.ID,
		Address:      inv.Address,
		AmountSat:    inv.AmountSat,
		RequiredConf: inv.RequiredConf,
		Status:       string(inv.Status),
		CreatedAt:    inv.CreatedAt.Format(time.RFC3339),
		ExpiresAt:    inv.ExpiresAt.Format(time.RFC3339),
	})
}

func (s *Server) getInvoice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()

	inv, err := s.store.Invoice(ctx, id)
	if errors.Is(err, payments.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no invoice with that id")
		return
	}
	if err != nil {
		s.fail(w, "load invoice", err)
		return
	}

	obs, err := s.store.Observations(ctx, inv.ID)
	if err != nil {
		s.fail(w, "load observations", err)
		return
	}

	writeJSON(w, http.StatusOK, invoiceResponse{
		ID:            inv.ID,
		Address:       inv.Address,
		AmountSat:     inv.AmountSat,
		PaidSat:       payments.Paid(obs),
		RequiredConf:  inv.RequiredConf,
		Confirmations: payments.MinConfirmations(obs),
		Status:        string(inv.Status),
		CreatedAt:     inv.CreatedAt.Format(time.RFC3339),
		ExpiresAt:     inv.ExpiresAt.Format(time.RFC3339),
	})
}

// fail logs the real cause and tells the caller only that something broke.
// The internal error may name a wallet, a file path or a node — none of which
// belongs in a merchant-facing response.
func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	s.log.Error("request failed", "op", what, "err", err)
	writeError(w, http.StatusInternalServerError, "could not "+what)
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// Headers are already out; nothing useful is left to do but note it.
		fmt.Fprintln(w)
	}
}

func writeError(w http.ResponseWriter, code int, message string) {
	writeJSON(w, code, map[string]string{"error": message})
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing means the system is in a state where inventing a
		// weaker id would be the smaller of our problems — but an invoice id
		// must never collide, so refuse rather than fall back to time.
		panic("crypto/rand unavailable: " + err.Error())
	}
	return "inv_" + strings.ToLower(hex.EncodeToString(b[:]))
}
