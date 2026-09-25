package wallet

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/syntr0x79/btcpay-gate/internal/rpc"
)

// fakeNode enforces the one rule of core's that this package has to live
// with, because a fake more permissive than the node is how the bug below
// survived: the previous version accepted any range, so the test suite proved
// behaviour bitcoind refuses outright.
//
// Measured on core 28.0, regtest:
//   - import [0,50] on a fresh wallet succeeds, and leaves the wallet
//     watching [0,999] — core widens an active descriptor to its keypool;
//   - every later import must COVER that range: [0,50] again, [50,100] and
//     [1000,2000] all fail with -8, and only [0,N>=999] succeeds.
type fakeNode struct {
	created   string
	imports   [][2]int
	derived   []int
	importErr error

	// keypool is core's widening of a freshly imported descriptor. Zero
	// disables it, which keeps the older tests about this package's own
	// arithmetic readable; the tests that are about core's rule set it.
	keypool int

	imported bool
	rangeEnd int
}

func (f *fakeNode) ActiveRangeEnd(_ context.Context) (int, error) {
	return f.rangeEnd, nil
}

func (f *fakeNode) CreateWatchOnlyWallet(_ context.Context, name string) error {
	f.created = name
	return nil
}

func (f *fakeNode) GetDescriptorInfo(_ context.Context, d string) (rpc.DescriptorInfo, error) {
	return rpc.DescriptorInfo{Descriptor: d + "#checksum", Checksum: "checksum"}, nil
}

func (f *fakeNode) ImportRangedDescriptor(_ context.Context, _ string, from, to int) error {
	if f.importErr != nil {
		return f.importErr
	}
	if f.imported && (from != 0 || to < f.rangeEnd) {
		return &rpc.Error{Code: -8, Message: fmt.Sprintf(
			"new range must include current range = [0,%d]", f.rangeEnd)}
	}
	f.imports = append(f.imports, [2]int{from, to})
	f.imported = true
	f.rangeEnd = to
	if f.keypool > f.rangeEnd {
		f.rangeEnd = f.keypool
	}
	return nil
}

func (f *fakeNode) DeriveAddresses(_ context.Context, _ string, from, _ int) ([]string, error) {
	f.derived = append(f.derived, from)
	return []string{fmt.Sprintf("bcrt1qderived%d", from)}, nil
}

const xpubDescriptor = "wpkh([d34db33f/84h/1h/0h]tpubDEADBEEF/0/*)"

func TestOpenImportsTheDescriptorWithAChecksum(t *testing.T) {
	node := &fakeNode{}
	w, err := Open(context.Background(), node, Config{
		Descriptor: xpubDescriptor, WalletName: "payments", InitialRange: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if node.created != "payments" {
		t.Fatalf("wallet name = %q", node.created)
	}
	if !strings.HasSuffix(w.Descriptor(), "#checksum") {
		t.Fatalf("descriptor = %q, want the checksummed form — core rejects it otherwise", w.Descriptor())
	}
	if len(node.imports) != 1 || node.imports[0] != [2]int{0, 10} {
		t.Fatalf("imports = %v, want one [0 10]", node.imports)
	}
}

func TestPrivateDescriptorIsRefused(t *testing.T) {
	// A descriptor with spending keys on an internet-facing host is the
	// failure this whole design exists to prevent. Fail at startup, loudly.
	for _, d := range []string{
		"wpkh([d34db33f/84h/0h/0h]xprvSECRET/0/*)",
		"wpkh(tprvSECRET/0/*)",
	} {
		if _, err := Open(context.Background(), &fakeNode{}, Config{Descriptor: d, WalletName: "w"}); err == nil {
			t.Fatalf("descriptor %q was accepted; it contains a private key", d)
		}
	}
}

func TestUnrangedDescriptorIsRefused(t *testing.T) {
	// Without /* the descriptor yields a single address, so every invoice
	// would share one — and a payment for one would settle another.
	_, err := Open(context.Background(), &fakeNode{}, Config{
		Descriptor: "wpkh([d34db33f/84h/0h/0h]tpubDEADBEEF/0/0)", WalletName: "w",
	})
	if err == nil {
		t.Fatal("an unranged descriptor was accepted")
	}
}

func TestAddressAtIsDeterministicPerIndex(t *testing.T) {
	node := &fakeNode{}
	w, err := Open(context.Background(), node, Config{
		Descriptor: xpubDescriptor, WalletName: "payments", InitialRange: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	first, err := w.AddressAt(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	again, err := w.AddressAt(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Fatalf("index 3 gave %q then %q — addresses must be reproducible after a restore", first, again)
	}
	other, err := w.AddressAt(ctx, 4)
	if err != nil {
		t.Fatal(err)
	}
	if other == first {
		t.Fatal("different indexes produced the same address")
	}
}

func TestRangeIsExtendedBeforeItRunsOut(t *testing.T) {
	// Handing out an address core is not watching creates an invoice that can
	// be paid and will never be seen.
	node := &fakeNode{}
	w, err := Open(context.Background(), node, Config{
		Descriptor: xpubDescriptor, WalletName: "payments", InitialRange: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if _, err := w.AddressAt(ctx, 9); err != nil {
		t.Fatal(err)
	}
	if len(node.imports) != 1 {
		t.Fatalf("index 9 is inside the initial range, no import expected, got %v", node.imports)
	}

	if _, err := w.AddressAt(ctx, 10); err != nil {
		t.Fatal(err)
	}
	// [0 20], not [10 20]: core refuses any import that does not cover the
	// range already active, so the new stretch alone is never accepted. This
	// line used to read [10 20] and passed only because the fake was more
	// permissive than the node.
	if len(node.imports) != 2 || node.imports[1] != [2]int{0, 20} {
		t.Fatalf("imports = %v, want the range extended with [0 20]", node.imports)
	}

	// A jump far past the end must still be covered.
	if _, err := w.AddressAt(ctx, 57); err != nil {
		t.Fatal(err)
	}
	last := node.imports[len(node.imports)-1]
	if last[0] != 0 || last[1] <= 57 {
		t.Fatalf("last import %v does not cover [0 57]", last)
	}
}

func TestAddressFailsWhenTheRangeCannotBeExtended(t *testing.T) {
	node := &fakeNode{}
	w, err := Open(context.Background(), node, Config{
		Descriptor: xpubDescriptor, WalletName: "payments", InitialRange: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	node.importErr = fmt.Errorf("node unreachable")

	// Better to fail the invoice than to hand out an address nobody watches.
	if _, err := w.AddressAt(context.Background(), 5); err == nil {
		t.Fatal("expected an error when the watched range cannot be extended")
	}
}

// TestRestartSurvivesTheKeypoolWidening.
//
// The stand's failure. ADDRESS_GAP=50 there, core widens the descriptor to
// [0,999] on the first import, and the second start asks for [0,50] again —
// which core refuses. Open returned that error and the process died in a
// restart loop, so restarting paygate on a live stand was impossible.
func TestRestartSurvivesTheKeypoolWidening(t *testing.T) {
	node := &fakeNode{keypool: 1000}
	cfg := Config{Descriptor: xpubDescriptor, WalletName: "payments", InitialRange: 50}

	if _, err := Open(context.Background(), node, cfg); err != nil {
		t.Fatalf("first open: %v", err)
	}
	if _, err := Open(context.Background(), node, cfg); err != nil {
		t.Fatalf("second open — a plain restart against the same node: %v", err)
	}
	// And it comes back knowing the node's real reach, not the 50 it asked
	// for: an invoice at index 900 is inside what core watches, so it must
	// not trigger an extension.
	w, err := Open(context.Background(), node, cfg)
	if err != nil {
		t.Fatalf("third open: %v", err)
	}
	before := len(node.imports)
	if _, err := w.AddressAt(context.Background(), 900); err != nil {
		t.Fatalf("index 900 is inside the watched range, yet: %v", err)
	}
	if len(node.imports) != before {
		t.Errorf("index 900 caused an import; the wallet kept its own 50 instead of "+
			"the [0,%d] the node actually watches", node.rangeEnd)
	}
}

// TestExtensionCoversWhatTheNodeAlreadyWatches.
//
// The half that was never noticed, and it is the worse one: extension asked
// for the NEW stretch only ([1000,2000]), which core refuses whatever the
// numbers are. So the guard against handing out an unwatched address had
// never once worked — with the shipped ADDRESS_GAP of 1000 it would have
// started failing every invoice from the thousand-and-first.
func TestExtensionCoversWhatTheNodeAlreadyWatches(t *testing.T) {
	node := &fakeNode{keypool: 1000}
	w, err := Open(context.Background(), node, Config{
		Descriptor: xpubDescriptor, WalletName: "payments", InitialRange: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := w.AddressAt(context.Background(), 1000); err != nil {
		t.Fatalf("invoice at index 1000 failed: %v", err)
	}
	last := node.imports[len(node.imports)-1]
	if last[0] != 0 {
		t.Errorf("extension imported %v; core accepts only ranges starting at 0", last)
	}
	if last[1] <= 1000 {
		t.Errorf("extension imported %v, which does not cover index 1000", last)
	}
}

// TestNoPointlessImportWhenTheNodeAlreadyCoversUs.
//
// Once core has widened the range past what we ask for, re-importing the same
// descriptor changes nothing on the node. Doing it anyway on every start is
// harmless but dishonest: it reads as "the range was set up here", and the
// next person debugging a range problem would look in the wrong place.
func TestNoPointlessImportWhenTheNodeAlreadyCoversUs(t *testing.T) {
	node := &fakeNode{keypool: 1000}
	cfg := Config{Descriptor: xpubDescriptor, WalletName: "payments", InitialRange: 50}
	if _, err := Open(context.Background(), node, cfg); err != nil {
		t.Fatal(err)
	}
	after := len(node.imports)

	w, err := Open(context.Background(), node, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(node.imports) != after {
		t.Errorf("a restart issued %d extra import(s) though the node already watches "+
			"[0,%d] and only 50 was asked for", len(node.imports)-after, node.rangeEnd)
	}
	// And it must still know how far it is covered, or the first invoice
	// would trigger an extension that is not needed.
	if _, err := w.AddressAt(context.Background(), 900); err != nil {
		t.Fatalf("index 900 is inside what the node watches, yet: %v", err)
	}
	if len(node.imports) != after {
		t.Errorf("index 900 caused an import; the wallet forgot the node's real range")
	}
}
