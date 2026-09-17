package wallet

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/syntr0x79/btcpay-gate/internal/rpc"
)

type fakeNode struct {
	created   string
	imports   [][2]int
	derived   []int
	importErr error
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
	f.imports = append(f.imports, [2]int{from, to})
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
	if len(node.imports) != 2 || node.imports[1] != [2]int{10, 20} {
		t.Fatalf("imports = %v, want the range extended to [10 20]", node.imports)
	}

	// A jump far past the end must still be covered.
	if _, err := w.AddressAt(ctx, 57); err != nil {
		t.Fatal(err)
	}
	last := node.imports[len(node.imports)-1]
	if last[1] <= 57 {
		t.Fatalf("last import %v does not cover index 57", last)
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
