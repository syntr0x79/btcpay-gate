package rpc

import (
	"context"
	"errors"
	"math"
	"strings"
)

// Sat is an amount in satoshi. Core speaks BTC as a JSON number, which is a
// float — the one place in a payment system where a float is unacceptable.
// Every amount crossing this boundary is converted immediately and never
// travels as a float again.
type Sat int64

// btcToSat rounds rather than truncates: 0.1 BTC arrives as 0.100000000000000005
// often enough that truncation loses a satoshi, and losing satoshis in an
// accounting system is how invoices silently become underpaid.
func btcToSat(btc float64) Sat {
	return Sat(math.Round(btc * 1e8))
}

type BlockchainInfo struct {
	Chain  string `json:"chain"`
	Blocks int64  `json:"blocks"`
}

func (c *Client) GetBlockchainInfo(ctx context.Context) (BlockchainInfo, error) {
	var out BlockchainInfo
	err := c.Call(ctx, &out, "getblockchaininfo")
	return out, err
}

func (c *Client) GetBestBlockHash(ctx context.Context) (string, error) {
	var out string
	err := c.Call(ctx, &out, "getbestblockhash")
	return out, err
}

// CreateWatchOnlyWallet creates a descriptor wallet with no private keys.
// disable_private_keys is the whole point: the node cannot sign, so a
// compromised node cannot move funds.
func (c *Client) CreateWatchOnlyWallet(ctx context.Context, name string) error {
	// name, disable_private_keys, blank, passphrase, avoid_reuse, descriptors, load_on_startup
	err := c.Call(ctx, nil, "createwallet", name, true, true, "", false, true, true)
	if err == nil || !isAlreadyExists(err) {
		return err
	}

	// The wallet is already on disk, from an earlier run of this gateway.
	//
	// load_on_startup is repeated here and not only on createwallet: a wallet
	// that exists says nothing about whether core still has it in the startup
	// list. On shop-pl it did not, so every restart of the node left the node
	// walletless and the gateway polling into the void until someone restarted
	// the gateway. Setting it here is what makes the node recover on its own.
	err = c.Call(ctx, nil, "loadwallet", name, true)
	if isAlreadyLoaded(err) {
		// Which is exactly what the startup list above produces: the node
		// loaded the wallet before we asked. Treating -35 as fatal is how
		// adding -wallet= to the node put this process in a restart loop on
		// 2026-09-22 — the two mechanisms were mutually exclusive, and they
		// should not have been.
		return nil
	}
	return err
}

func isAlreadyExists(err error) bool {
	var re *Error
	if errors.As(err, &re) {
		// -4: wallet already exists. Message matching as a fallback, because
		// the code has moved between core releases.
		return re.Code == -4 || strings.Contains(strings.ToLower(re.Message), "already exists")
	}
	return false
}

// isAlreadyLoaded reports core's -35, RPC_WALLET_ALREADY_LOADED. Narrow by
// design: -4 from loadwallet means the wallet is on disk but unopenable (a
// pruned node past the wallet's last sync point, say), and that must still
// stop the gateway rather than be started around.
func isAlreadyLoaded(err error) bool {
	var re *Error
	if errors.As(err, &re) {
		return re.Code == -35 || strings.Contains(strings.ToLower(re.Message), "already loaded")
	}
	return false
}

// IsWalletNotLoaded reports core's -18, RPC_WALLET_NOT_FOUND: the node has no
// wallet by that name open, so every wallet-scoped call will fail the same way
// until something loads it. Distinguishing this from a timeout is what lets
// the health state say "the wallet is gone" instead of "the node is slow".
func IsWalletNotLoaded(err error) bool {
	var re *Error
	if errors.As(err, &re) {
		return re.Code == -18
	}
	return false
}

type DescriptorInfo struct {
	Descriptor string `json:"descriptor"`
	Checksum   string `json:"checksum"`
}

// GetDescriptorInfo returns the descriptor with its checksum appended. Core
// refuses descriptors without a checksum, and computing one by hand is a
// pointless way to introduce a typo.
func (c *Client) GetDescriptorInfo(ctx context.Context, descriptor string) (DescriptorInfo, error) {
	var out DescriptorInfo
	err := c.Call(ctx, &out, "getdescriptorinfo", descriptor)
	return out, err
}

type importRequest struct {
	Desc      string `json:"desc"`
	Active    bool   `json:"active"`
	Range     []int  `json:"range,omitempty"`
	Timestamp any    `json:"timestamp"`
	Internal  bool   `json:"internal"`
	Label     string `json:"label,omitempty"`
}

type ImportResult struct {
	Success bool   `json:"success"`
	Error   *Error `json:"error"`
}

// ImportRangedDescriptor registers a ranged descriptor as the wallet's active
// receiving descriptor, so core derives and watches addresses itself.
//
// timestamp "now" tells core not to rescan history. That is correct for a
// freshly created payment wallet and wrong for one being restored — the
// caller decides, but the default here is the safe one for new deployments,
// because an accidental full rescan on a mainnet node blocks the wallet for
// hours.
func (c *Client) ImportRangedDescriptor(ctx context.Context, descriptor string, from, to int) error {
	var out []ImportResult
	req := importRequest{
		Desc:      descriptor,
		Active:    true,
		Range:     []int{from, to},
		Timestamp: "now",
		Internal:  false,
	}
	if err := c.Call(ctx, &out, "importdescriptors", []importRequest{req}); err != nil {
		return err
	}
	for _, r := range out {
		if !r.Success {
			if r.Error != nil {
				return r.Error
			}
			return errors.New("importdescriptors: not successful")
		}
	}
	return nil
}

// DeriveAddresses asks core to derive addresses from a ranged descriptor.
// Deriving them here rather than calling getnewaddress means the mapping from
// index to address is reproducible: the same xpub and index always produce the
// same address, so an invoice can be re-derived after a database restore.
func (c *Client) DeriveAddresses(ctx context.Context, descriptor string, from, to int) ([]string, error) {
	var out []string
	err := c.Call(ctx, &out, "deriveaddresses", descriptor, []int{from, to})
	return out, err
}

// Transaction is one entry from listsinceblock.
type Transaction struct {
	Address       string  `json:"address"`
	Category      string  `json:"category"`
	AmountBTC     float64 `json:"amount"`
	Confirmations int64   `json:"confirmations"`
	TxID          string  `json:"txid"`
	BlockHash     string  `json:"blockhash"`
	BlockHeight   int64   `json:"blockheight"`
	Vout          int64   `json:"vout"`
	Time          int64   `json:"time"`
	// Set by core when a conflicting transaction was mined instead of this
	// one. A non-empty list means this transaction is not coming back.
	WalletConflicts []string `json:"walletconflicts"`
}

func (t Transaction) Amount() Sat { return btcToSat(t.AmountBTC) }

// IsReceive filters out the change and send entries core also returns.
func (t Transaction) IsReceive() bool { return t.Category == "receive" }

type SinceBlock struct {
	Transactions []Transaction `json:"transactions"`
	// Removed holds transactions that were in the chain at the old block and
	// are no longer — the reorg signal. Core only populates it when
	// include_removed is true.
	Removed   []Transaction `json:"removed"`
	LastBlock string        `json:"lastblock"`
}

// ListSinceBlock returns everything that happened after blockHash.
//
// This, not a push notification, is the source of truth. ZMQ delivers events
// once and never again: a process that is restarting when a block arrives
// misses it permanently. A cursor into the chain is replayable by
// construction, so a crash costs a few seconds of lag instead of a payment.
//
// An empty blockHash means "from genesis", which on a fresh watch-only wallet
// is cheap because it has no history.
func (c *Client) ListSinceBlock(ctx context.Context, blockHash string, minConf int) (SinceBlock, error) {
	var out SinceBlock
	// blockhash, target_confirmations, include_watchonly, include_removed
	err := c.Call(ctx, &out, "listsinceblock", blockHash, minConf, true, true)
	return out, err
}

// --- regtest helpers -------------------------------------------------------
// Only used by integration tests, kept here so the RPC surface lives in one
// place.

func (c *Client) GenerateToAddress(ctx context.Context, blocks int, address string) ([]string, error) {
	var out []string
	err := c.Call(ctx, &out, "generatetoaddress", blocks, address)
	return out, err
}

func (c *Client) SendToAddress(ctx context.Context, address string, btc float64) (string, error) {
	var out string
	err := c.Call(ctx, &out, "sendtoaddress", address, btc)
	return out, err
}

func (c *Client) InvalidateBlock(ctx context.Context, blockHash string) error {
	return c.Call(ctx, nil, "invalidateblock", blockHash)
}

func (c *Client) GetNewAddress(ctx context.Context) (string, error) {
	var out string
	err := c.Call(ctx, &out, "getnewaddress")
	return out, err
}
