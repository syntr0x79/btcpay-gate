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

	"github.com/syntr0x79/btcpay-gate/internal/payments"
)

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
	log         *slog.Logger
	now         func() time.Time
}

type Config struct {
	DefaultConfirmations int64
	DefaultTTL           time.Duration
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
		log:         log,
		now:         time.Now,
	}
}

func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /invoices", s.createInvoice)
	mux.HandleFunc("GET /invoices/{id}", s.getInvoice)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok\n"))
	})
	return mux
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
