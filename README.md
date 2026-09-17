# btcpay-gate

[![tests](https://github.com/syntr0x79/btcpay-gate/actions/workflows/tests.yml/badge.svg)](https://github.com/syntr0x79/btcpay-gate/actions/workflows/tests.yml)

A Bitcoin payment gateway on top of bitcoin-core. The interesting part is not taking payments — it is what happens when the chain takes one back.

```bash
curl -XPOST localhost:8080/invoices -d '{"amount_sat": 150000, "required_conf": 2}'
{
  "id": "inv_9f2c…", "address": "bcrt1q…", "amount_sat": 150000,
  "required_conf": 2, "status": "pending", "expires_at": "2026-09-17T18:05:00Z"
}
```

The service watches the chain, moves the invoice through its lifecycle and calls the merchant's webhook on every change.

## Three decisions worth defending

**The node cannot spend.** Core runs a watch-only descriptor wallet built from an **xpub**; private keys are never on this host and never on the node. Compromising the service exposes which addresses are being watched, not the funds. A descriptor containing `xprv` is refused at startup rather than quietly accepted — the check is three lines and it closes the failure this whole design exists to prevent.

Addresses are derived per index from that descriptor, not requested with `getnewaddress`. The mapping is reproducible: after a database restore, every invoice's address can be recomputed instead of lost.

**Polling, not ZMQ.** A push notification is delivered once. A process that is restarting, wedged or being deployed when a block arrives never learns about it, and nothing in the system knows a payment was missed. `listsinceblock` from a persisted cursor is replayable by construction — on start, ask "what happened since this block?" and core answers with everything, including the blocks you were not running for. The cost is latency bounded by the poll interval, against a ten-minute block time.

**Status changes and their webhooks commit together.** A change is written to the invoice and to an outbox table in one transaction. A webhook therefore cannot be lost by a crash, and cannot be sent for a change that was rolled back. Delivery is at-least-once with an idempotency key, which is the only thing an HTTP callback can honestly promise.

## The lifecycle

```
pending ──payment in mempool──▶ seen ──mined──▶ confirming ──threshold──▶ confirmed
   │                              │                  │
   │ expiry                       └──────┬───────────┘
   ▼                                     │ block invalidated / double spend
expired                                  ▼
                                      reorged ──mined again──▶ confirming …
```

Three rules that took a while to get right and are pinned by tests:

- **Expiry only applies while nothing has been received.** Once money is in flight the clock stops. Expiring an invoice somebody already paid is the worst outcome available to this system.
- **Underpayment is judged only at the confirmation threshold.** Below it the total can still grow, and calling an invoice underpaid while a second transaction sits in the mempool is a false alarm.
- **`confirmed` is sticky, `reorged` is not.** Once an order has shipped, a later reorg cannot un-ship it — that is a manual decision. But a transaction dropped by one reorg and mined by the next block must bring the invoice back, or a customer who genuinely paid is stranded.

## The bug the integration tests found

The unit tests were green. Against a real node, every invoice requiring more than one confirmation froze at exactly one.

`listsinceblock` reports what happened *after* the cursor. With `target_confirmations=1`, core returns the current tip as `lastblock` — so the cursor advanced onto the block holding the payment, the payment stopped being reported, and its confirmation count in the database never moved again.

That parameter is not a filter, it is how far back `lastblock` is placed. The cursor has to **lag** the tip by more than the deepest threshold any invoice can ask for. Twelve blocks by default, `WithWindow` to raise it. Re-reading the same blocks every poll is harmless because observations are keyed on `(txid, vout)` and upserted — the same property that makes a crash mid-catch-up safe.

There is now a unit test asserting the cursor lags, so the fix cannot be undone by accident.

## Tests

```
$ go test ./...              # 52 unit tests, nothing to install
$ make test-regtest          # 8 integration tests against bitcoind in docker
```

The integration suite wipes the chain first and runs against real bitcoin-core:

| | |
|---|---|
| `TestPaymentReachesConfirmed` | mempool → 1 conf → 2 conf |
| `TestUnderpaymentIsReported` | short payment, then the remainder settles it |
| `TestReorgTakesThePaymentBack` | `invalidateblock`, and the invoice stops counting money that left the chain |
| `TestReorgedPaymentRecoversWhenMinedAgain` | the payment returns and the invoice completes |
| `TestRestartResumesFromTheCursor` | blocks mined while the service is down are not missed |
| `TestWebhookFiresOnStatusChange` | signature verified by the receiver |
| `TestAddressesAreNeverReused` | |
| `TestWatchOnlyWalletCannotSpend` | the security claim above, asserted rather than described |

Two lessons are baked into that suite. Mining a replacement block to the *same* address in the same second reproduces the invalidated block bit for bit, and core rejects it — a regtest trap that looks like a node failure. And a mempool transaction does not reach the watch-only wallet the instant `sendtoaddress` returns, so the tests wait on a condition rather than sleeping.

## Webhooks

```
POST /your/endpoint
X-Webhook-Timestamp: 1789640000
X-Webhook-Signature:  <hex hmac-sha256 over "timestamp.body">
X-Idempotency-Key:    42

{"event_id":42,"invoice_id":"inv_9f2c…","status":"confirmed","paid_sat":150000,"confirmations":2}
```

The timestamp is inside the signed material, not merely alongside it — sign the body alone and a captured request replays forever. Retries back off exponentially and stop at an hour: uncapped doubling reaches "next week" after a dozen failures, and a merchant who has just fixed their endpoint would wait a week. After the final attempt the row stays, with the reason, because a dropped webhook that leaves no trace is indistinguishable from one that was never generated.

`webhook.Verify` is the receiver-side check, with constant-time comparison and a freshness window.

## Running it

```bash
export BITCOIND_RPC_URL=http://127.0.0.1:8332
export BITCOIND_RPC_USER=… BITCOIND_RPC_PASSWORD=…
export WALLET_DESCRIPTOR='wpkh([fingerprint/84h/0h/0h]xpub…/0/*)'
export WEBHOOK_URL=https://shop.example.com/hooks/btc
export WEBHOOK_SECRET=…
go run ./cmd/paygate
```

`WEBHOOK_SECRET` is mandatory once `WEBHOOK_URL` is set — unsigned callbacks anyone can forge are not a reasonable default. Other knobs: `REQUIRED_CONFIRMATIONS`, `INVOICE_TTL`, `POLL_INTERVAL`, `ADDRESS_GAP`, `DB_PATH`, `LISTEN_ADDR`.

## What this is not

Not a production payment processor. There is no fee handling, no refunds, no HD account separation per merchant, no authentication on the HTTP API, and SQLite means one instance. It is a correct core — lifecycle, reorg handling, durable delivery — with the sharp edges documented rather than hidden.

Written to answer a question I could not answer well in an interview: *how do you handle a reorg?*

## Author

Dmitry Buravtsov — platform and infrastructure engineer.
[github.com/syntr0x79](https://github.com/syntr0x79)

## License

MIT — see [LICENSE](LICENSE).
