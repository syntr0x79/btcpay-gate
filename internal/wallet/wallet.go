// Package wallet turns an extended public key into receiving addresses.
//
// The private keys are never here, and never on the node either. Core runs a
// watch-only descriptor wallet: it can derive addresses and see incoming
// payments, and it cannot sign anything. Compromising this service — or the
// node it talks to — exposes which addresses are being watched, not the funds.
//
// Spending happens elsewhere, from a wallet that holds the matching xprv, on a
// machine that does not accept HTTP requests.
package wallet

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/syntr0x79/btcpay-gate/internal/rpc"
)

type Node interface {
	CreateWatchOnlyWallet(ctx context.Context, name string) error
	GetDescriptorInfo(ctx context.Context, descriptor string) (rpc.DescriptorInfo, error)
	ImportRangedDescriptor(ctx context.Context, descriptor string, from, to int) error
	DeriveAddresses(ctx context.Context, descriptor string, from, to int) ([]string, error)
}

type Wallet struct {
	node       Node
	descriptor string // with checksum

	mu     sync.Mutex
	gapEnd int // highest index currently imported
	step   int
}

type Config struct {
	// Descriptor without a checksum, e.g.
	//   wpkh([fingerprint/84h/0h/0h]xpub.../0/*)
	// The /* is what makes it ranged; a descriptor without it produces one
	// address and every invoice would share it.
	Descriptor string
	// WalletName is the wallet core creates. Must not already hold keys.
	WalletName string
	// InitialRange is how many addresses to import up front.
	InitialRange int
}

// Open creates the watch-only wallet if needed and imports the descriptor.
func Open(ctx context.Context, node Node, cfg Config) (*Wallet, error) {
	if !strings.Contains(cfg.Descriptor, "/*") {
		return nil, fmt.Errorf("descriptor must be ranged (contain /*), got %q", cfg.Descriptor)
	}
	if strings.Contains(cfg.Descriptor, "xprv") || strings.Contains(cfg.Descriptor, "tprv") {
		// A private descriptor here would put spending keys on an internet-facing
		// host. Refuse loudly rather than quietly accept it.
		return nil, fmt.Errorf("descriptor contains a private key; this service must only ever see an xpub")
	}
	if cfg.InitialRange <= 0 {
		cfg.InitialRange = 1000
	}

	if err := node.CreateWatchOnlyWallet(ctx, cfg.WalletName); err != nil {
		return nil, fmt.Errorf("create watch-only wallet: %w", err)
	}

	info, err := node.GetDescriptorInfo(ctx, cfg.Descriptor)
	if err != nil {
		return nil, fmt.Errorf("descriptor info: %w", err)
	}
	checksummed := info.Descriptor

	if err := node.ImportRangedDescriptor(ctx, checksummed, 0, cfg.InitialRange); err != nil {
		return nil, fmt.Errorf("import descriptor: %w", err)
	}

	return &Wallet{
		node:       node,
		descriptor: checksummed,
		gapEnd:     cfg.InitialRange,
		step:       cfg.InitialRange,
	}, nil
}

// AddressAt derives the address for one index.
//
// Deriving by index rather than calling getnewaddress is what makes the
// mapping reproducible: the same xpub and the same index always yield the same
// address. After a database restore, every invoice's address can be recomputed
// instead of being lost.
func (w *Wallet) AddressAt(ctx context.Context, index int) (string, error) {
	if err := w.ensureWatched(ctx, index); err != nil {
		return "", err
	}
	addrs, err := w.node.DeriveAddresses(ctx, w.descriptor, index, index)
	if err != nil {
		return "", fmt.Errorf("derive address %d: %w", index, err)
	}
	if len(addrs) == 0 {
		return "", fmt.Errorf("derive address %d: node returned nothing", index)
	}
	return addrs[0], nil
}

// ensureWatched extends the imported range before it runs out.
//
// Deriving an address core is not watching produces an invoice that can be
// paid and will never be seen — the worst failure this service has. The check
// is cheap; the failure is not.
func (w *Wallet) ensureWatched(ctx context.Context, index int) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if index < w.gapEnd {
		return nil
	}
	next := w.gapEnd + w.step
	for index >= next {
		next += w.step
	}
	if err := w.node.ImportRangedDescriptor(ctx, w.descriptor, w.gapEnd, next); err != nil {
		return fmt.Errorf("extend watched range to %d: %w", next, err)
	}
	w.gapEnd = next
	return nil
}

// Descriptor returns the checksummed descriptor in use, for diagnostics.
func (w *Wallet) Descriptor() string { return w.descriptor }
