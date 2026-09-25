// Command paygate runs the payment gateway: HTTP API, chain monitor and
// webhook sender in one process.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/syntr0x79/btcpay-gate/internal/api"
	"github.com/syntr0x79/btcpay-gate/internal/health"
	"github.com/syntr0x79/btcpay-gate/internal/monitor"
	"github.com/syntr0x79/btcpay-gate/internal/payments"
	"github.com/syntr0x79/btcpay-gate/internal/rpc"
	"github.com/syntr0x79/btcpay-gate/internal/wallet"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := run(log); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	node := rpc.New(cfg.rpcURL, cfg.rpcUser, cfg.rpcPassword, rpc.WithWallet(cfg.walletName))

	info, err := node.GetBlockchainInfo(ctx)
	if err != nil {
		return err
	}
	log.Info("connected to bitcoind", "chain", info.Chain, "blocks", info.Blocks)

	w, err := wallet.Open(ctx, node, wallet.Config{
		Descriptor:   cfg.descriptor,
		WalletName:   cfg.walletName,
		InitialRange: cfg.addressGap,
	})
	if err != nil {
		return err
	}

	store, err := payments.Open(cfg.dbPath)
	if err != nil {
		return err
	}
	defer store.Close()

	// One health state, written by the monitor and read by the HTTP layer, so
	// that /healthz and /metrics answer from what the poll loop actually
	// experienced rather than from the fact that a listener is bound.
	hl := health.New(cfg.maxPollAge)

	mon := monitor.New(node, store, cfg.pollInterval, log).WithHealth(hl)
	sender := newSender(store, cfg, log)
	apiSrv := api.New(store, w, api.Config{
		DefaultConfirmations: cfg.requiredConf,
		DefaultTTL:           cfg.invoiceTTL,
		Health:               hl,
	}, log)
	server := &http.Server{
		Addr:              cfg.listen,
		Handler:           apiSrv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Metrics on their own listener, like the shop's METRICS_LISTEN_ADDR: this
	// is the only socket published to the monitoring mesh, and it is published
	// without authentication. The invoice API stays off it.
	var metricsSrv *http.Server
	if cfg.metricsListen != "" {
		metricsSrv = &http.Server{
			Addr:              cfg.metricsListen,
			Handler:           apiSrv.MetricsRoutes(),
			ReadHeaderTimeout: 10 * time.Second,
		}
	}

	errs := make(chan error, 4)
	go func() { errs <- mon.Run(ctx) }()
	go func() { errs <- sender.Run(ctx) }()
	go func() {
		log.Info("http listening", "addr", cfg.listen)
		err := server.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errs <- err
	}()
	if metricsSrv != nil {
		go func() {
			log.Info("metrics listening", "addr", cfg.metricsListen)
			err := metricsSrv.ListenAndServe()
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			errs <- err
		}()
	}

	select {
	case <-ctx.Done():
	case err := <-errs:
		if err != nil && !errors.Is(err, context.Canceled) {
			stop()
			shutdown(server, metricsSrv, log)
			return err
		}
	}

	shutdown(server, metricsSrv, log)
	return nil
}

func shutdown(server, metrics *http.Server, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Error("http shutdown", "err", err)
	}
	if metrics != nil {
		if err := metrics.Shutdown(ctx); err != nil {
			log.Error("metrics shutdown", "err", err)
		}
	}
	log.Info("stopped")
}

type config struct {
	rpcURL        string
	rpcUser       string
	rpcPassword   string
	walletName    string
	descriptor    string
	dbPath        string
	listen        string
	metricsListen string
	webhookURL    string
	webhookSecret string
	pollInterval  time.Duration
	maxPollAge    time.Duration
	invoiceTTL    time.Duration
	requiredConf  int64
	addressGap    int
}

func loadConfig() (config, error) {
	c := config{
		rpcURL:      env("BITCOIND_RPC_URL", "http://127.0.0.1:18443"),
		rpcUser:     env("BITCOIND_RPC_USER", ""),
		rpcPassword: env("BITCOIND_RPC_PASSWORD", ""),
		walletName:  env("WALLET_NAME", "payments"),
		descriptor:  env("WALLET_DESCRIPTOR", ""),
		dbPath:      env("DB_PATH", "paygate.db"),
		listen:      env("LISTEN_ADDR", ":8080"),
		// Empty by default: a gateway nobody scrapes should not open a port.
		metricsListen: env("METRICS_LISTEN_ADDR", ""),
		webhookURL:    env("WEBHOOK_URL", ""),
		webhookSecret: env("WEBHOOK_SECRET", ""),
		pollInterval:  duration("POLL_INTERVAL", 10*time.Second),
		maxPollAge:    duration("HEALTH_MAX_POLL_AGE", health.DefaultMaxPollAge),
		invoiceTTL:    duration("INVOICE_TTL", 30*time.Minute),
		requiredConf:  int64(number("REQUIRED_CONFIRMATIONS", 2)),
		addressGap:    number("ADDRESS_GAP", 1000),
	}

	if c.descriptor == "" {
		return c, errors.New("WALLET_DESCRIPTOR is required: a ranged public descriptor, e.g. wpkh([fp/84h/1h/0h]tpub.../0/*)")
	}
	if c.rpcUser == "" || c.rpcPassword == "" {
		return c, errors.New("BITCOIND_RPC_USER and BITCOIND_RPC_PASSWORD are required")
	}
	// A webhook URL without a secret means unauthenticated callbacks that any
	// third party can forge. Refuse the combination rather than quietly
	// sending unsigned requests.
	if c.webhookURL != "" && c.webhookSecret == "" {
		return c, errors.New("WEBHOOK_SECRET is required when WEBHOOK_URL is set")
	}
	return c, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func number(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func duration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
