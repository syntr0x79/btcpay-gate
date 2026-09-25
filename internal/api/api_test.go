package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/syntr0x79/btcpay-gate/internal/health"
	"github.com/syntr0x79/btcpay-gate/internal/payments"
)

type fakeAddresses struct {
	err  error
	seen []int
}

func (f *fakeAddresses) AddressAt(_ context.Context, index int) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.seen = append(f.seen, index)
	return fmt.Sprintf("bcrt1qaddr%d", index), nil
}

func newServer(t *testing.T) (*Server, *payments.Store, *fakeAddresses) {
	t.Helper()
	st, err := payments.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	addrs := &fakeAddresses{}
	s := New(st, addrs, Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return s, st, addrs
}

func do(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, r)
	return w
}

func decode(t *testing.T, w *httptest.ResponseRecorder) invoiceResponse {
	t.Helper()
	var out invoiceResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return out
}

func TestCreateInvoice(t *testing.T) {
	s, _, _ := newServer(t)
	w := do(t, s, "POST", "/invoices", `{"amount_sat": 150000}`)

	if w.Code != http.StatusCreated {
		t.Fatalf("status %d, body %s", w.Code, w.Body)
	}
	inv := decode(t, w)
	if inv.ID == "" || !strings.HasPrefix(inv.ID, "inv_") {
		t.Fatalf("id = %q", inv.ID)
	}
	if inv.Address != "bcrt1qaddr0" {
		t.Fatalf("address = %q, want the first derived address", inv.Address)
	}
	if inv.AmountSat != 150000 {
		t.Fatalf("amount = %d", inv.AmountSat)
	}
	if inv.Status != "pending" {
		t.Fatalf("status = %q", inv.Status)
	}
	if inv.RequiredConf != 2 {
		t.Fatalf("required_conf = %d, want the default 2", inv.RequiredConf)
	}
}

func TestEachInvoiceGetsItsOwnAddress(t *testing.T) {
	// Address reuse would make two invoices indistinguishable on-chain, and a
	// payment for one would settle the other.
	s, _, addrs := newServer(t)

	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		w := do(t, s, "POST", "/invoices", `{"amount_sat": 1000}`)
		if w.Code != http.StatusCreated {
			t.Fatalf("status %d", w.Code)
		}
		inv := decode(t, w)
		if seen[inv.Address] {
			t.Fatalf("address %s handed out twice", inv.Address)
		}
		seen[inv.Address] = true
	}
	want := []int{0, 1, 2, 3, 4}
	if len(addrs.seen) != len(want) {
		t.Fatalf("derivation indexes = %v, want %v", addrs.seen, want)
	}
	for i, idx := range addrs.seen {
		if idx != want[i] {
			t.Fatalf("derivation indexes = %v, want %v", addrs.seen, want)
		}
	}
}

func TestCreateValidatesInput(t *testing.T) {
	s, _, _ := newServer(t)

	cases := []struct{ name, body string }{
		{"zero amount", `{"amount_sat": 0}`},
		{"negative amount", `{"amount_sat": -5}`},
		{"negative confirmations", `{"amount_sat": 1000, "required_conf": -1}`},
		{"zero ttl", `{"amount_sat": 1000, "ttl_seconds": 0}`},
		{"garbage", `not json`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := do(t, s, "POST", "/invoices", c.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400 (body %s)", w.Code, w.Body)
			}
		})
	}
}

func TestZeroConfirmationsIsAllowed(t *testing.T) {
	// Accepting 0-conf is a risk the merchant may choose to take for small
	// amounts. The service's job is to report honestly, not to forbid it.
	s, _, _ := newServer(t)
	w := do(t, s, "POST", "/invoices", `{"amount_sat": 1000, "required_conf": 0}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status %d, body %s", w.Code, w.Body)
	}
	if got := decode(t, w).RequiredConf; got != 0 {
		t.Fatalf("required_conf = %d, want 0", got)
	}
}

func TestGetInvoiceReportsProgress(t *testing.T) {
	s, st, _ := newServer(t)
	created := decode(t, do(t, s, "POST", "/invoices", `{"amount_sat": 100000, "required_conf": 2}`))

	ctx := context.Background()
	_, err := st.Apply(ctx, payments.Update{
		Observations: []payments.ObservationUpdate{{
			InvoiceID:   created.ID,
			Observation: payments.Observation{TxID: "tx1", AmountSat: 100000, Confirmations: 1},
		}},
		Cursor: "b1",
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	got := decode(t, do(t, s, "GET", "/invoices/"+created.ID, ""))
	if got.Status != "confirming" {
		t.Fatalf("status = %q, want confirming", got.Status)
	}
	if got.PaidSat != 100000 {
		t.Fatalf("paid_sat = %d", got.PaidSat)
	}
	if got.Confirmations != 1 {
		t.Fatalf("confirmations = %d", got.Confirmations)
	}
}

func TestUnknownInvoiceIs404(t *testing.T) {
	s, _, _ := newServer(t)
	w := do(t, s, "GET", "/invoices/inv_nope", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", w.Code)
	}
}

func TestAddressFailureDoesNotLeaveAHalfCreatedInvoice(t *testing.T) {
	// If the node cannot give us an address there is nothing to pay to, so the
	// invoice must not exist at all — an invoice without an address is a
	// customer staring at an empty payment page.
	s, st, addrs := newServer(t)
	addrs.err = fmt.Errorf("node unreachable")

	w := do(t, s, "POST", "/invoices", `{"amount_sat": 1000}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", w.Code)
	}

	open, err := st.OpenInvoices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Fatalf("%d invoices stored, want none", len(open))
	}
}

func TestInternalErrorsAreNotLeakedToTheCaller(t *testing.T) {
	s, _, addrs := newServer(t)
	addrs.err = fmt.Errorf("dial tcp 10.0.0.5:8332: connection refused")

	w := do(t, s, "POST", "/invoices", `{"amount_sat": 1000}`)
	if strings.Contains(w.Body.String(), "10.0.0.5") {
		t.Fatalf("internal detail leaked to the merchant: %s", w.Body)
	}
}

func TestIDsAreUnique(t *testing.T) {
	s, _, _ := newServer(t)
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		id := decode(t, do(t, s, "POST", "/invoices", `{"amount_sat": 1000}`)).ID
		if seen[id] {
			t.Fatalf("duplicate invoice id %s", id)
		}
		seen[id] = true
	}
}

// fakeHealth stands in for the chain monitor's view of itself.
type fakeHealth struct{ report health.Report }

func (f *fakeHealth) Report() health.Report { return f.report }

func healthServer(t *testing.T, r health.Report) *Server {
	t.Helper()
	st, err := payments.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, &fakeAddresses{}, Config{Health: &fakeHealth{report: r}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestHealthzGreenWhenMonitorIsReadingTheChain(t *testing.T) {
	s := healthServer(t, health.Report{OK: true, WalletLoaded: true, LastSuccess: time.Unix(1_700_000_000, 0)})
	w := do(t, s, "GET", "/healthz", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200; body %q", w.Code, w.Body.String())
	}
}

// The regression. /healthz used to be a constant 200, so a gateway whose poll
// loop had been dead for six hours still read as healthy from the outside —
// and docker, which probes this same handler, never restarted it.
func TestHealthzRedWhenPollLoopIsDead(t *testing.T) {
	s := healthServer(t, health.Report{OK: false, Reason: "wallet payments is not loaded"})
	w := do(t, s, "GET", "/healthz", "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503; body %q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "wallet payments is not loaded") {
		t.Errorf("body %q does not say why it is unhealthy", w.Body.String())
	}
}

// A gateway assembled without wiring the monitor in must not inherit the old
// always-green behaviour: forgetting the wiring has to be visible, and the
// safe direction to fail is red.
func TestHealthzRedWhenHealthIsNotWired(t *testing.T) {
	s, _, _ := newServer(t)
	if w := do(t, s, "GET", "/healthz", ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", w.Code)
	}
}

func TestMetricsExposeWalletAndPollAge(t *testing.T) {
	s := healthServer(t, health.Report{OK: true, WalletLoaded: true, LastSuccess: time.Unix(1_700_000_000, 0)})
	body := do(t, s, "GET", "/metrics", "").Body.String()

	for _, want := range []string{
		"paygate_wallet_loaded 1",
		"paygate_last_poll_timestamp_seconds 1700000000",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics missing %q:\n%s", want, body)
		}
	}
}

// Zero, not absent: an alert written as `paygate_wallet_loaded == 0` has to
// fire, and a series that disappears matches nothing.
func TestMetricsReportZeroForMissingWallet(t *testing.T) {
	s := healthServer(t, health.Report{OK: false, Reason: "wallet not loaded"})
	body := do(t, s, "GET", "/metrics", "").Body.String()

	if !strings.Contains(body, "paygate_wallet_loaded 0") {
		t.Errorf("/metrics does not report paygate_wallet_loaded 0:\n%s", body)
	}
	if !strings.Contains(body, "paygate_last_poll_timestamp_seconds 0") {
		t.Errorf("/metrics does not report a zero poll timestamp:\n%s", body)
	}
}

// The metrics listener is published to the monitoring mesh; the API listener
// is not. Anything reachable on that mux is therefore reachable unauthenticated
// by everything on the mesh, so it must carry metrics and nothing else —
// issuing invoices least of all.
func TestMetricsRoutesServeOnlyMetrics(t *testing.T) {
	s := healthServer(t, health.Report{OK: true, WalletLoaded: true})
	mux := s.MetricsRoutes()

	if w := serve(mux, "GET", "/metrics", ""); w.Code != http.StatusOK {
		t.Fatalf("/metrics on the metrics listener: status %d", w.Code)
	}
	for _, path := range []string{"/invoices", "/invoices/abc", "/healthz"} {
		if w := serve(mux, "POST", path, `{"amount_sat":1000}`); w.Code != http.StatusNotFound {
			t.Errorf("%s is reachable on the metrics listener: status %d", path, w.Code)
		}
	}
}

func serve(mux *http.ServeMux, method, path, body string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}
