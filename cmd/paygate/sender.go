package main

import (
	"log/slog"

	"github.com/syntr0x79/btcpay-gate/internal/payments"
	"github.com/syntr0x79/btcpay-gate/internal/webhook"
)

func newSender(store *payments.Store, cfg config, log *slog.Logger) *webhook.Sender {
	return webhook.New(store.DB(), webhook.Config{
		URL:    cfg.webhookURL,
		Secret: cfg.webhookSecret,
	}, log)
}
