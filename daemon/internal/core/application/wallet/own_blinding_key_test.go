package wallet

import (
	"bytes"
	"context"
	"encoding/hex"
	"testing"

	"github.com/aejkcs50/seqdex/daemon/internal/core/ports"
	"github.com/vulpemventures/go-elements/slip77"
)

// A wallet service whose only answer is Info: the accounts and their master blinding keys.
type infoOnlyWallet struct {
	ports.WalletService
	accounts []ports.WalletAccount
}
type infoOnlyWalletAPI struct {
	ports.Wallet
	w *infoOnlyWallet
}
type walletInfo struct {
	ports.WalletInfo
	accounts []ports.WalletAccount
}
type walletAccount struct {
	ports.WalletAccount
	label, namespace, mbk string
}

func (w *infoOnlyWallet) Wallet() ports.Wallet { return infoOnlyWalletAPI{w: w} }
func (a infoOnlyWalletAPI) Info(context.Context) (ports.WalletInfo, error) {
	return walletInfo{accounts: a.w.accounts}, nil
}
func (i walletInfo) GetAccounts() []ports.WalletAccount { return i.accounts }
func (a walletAccount) GetLabel() string                { return a.label }
func (a walletAccount) GetNamespace() string            { return a.namespace }
func (a walletAccount) GetMasterBlindingKey() string    { return a.mbk }

// The testnet maker's case: account sc-SEQ-USDX hands out transparent (tb1) addresses but
// holds a blinded tSEQ coin, which only blinded outputs can spend. Its outputs now get the
// SLIP77 key of their script, the key the wallet reads that script's blinded outputs with;
// before, they got none and every swap that selected the coin was refused.
func TestOwnBlindingKeyForATransparentAccount(t *testing.T) {
	mbk := "4cc1f2bba4a1bc1f2e1ab6a9b9e7c2d1f0a9b8c7d6e5f4a3b2c1d0e9f8a7b6c5"
	s := &Service{wallet: &infoOnlyWallet{accounts: []ports.WalletAccount{
		walletAccount{label: "sc-SEQ-USDX", namespace: "bip84-account3", mbk: mbk},
	}}}
	script, _ := hex.DecodeString("0014a9bd26f50fc1e2a5f2ac5ed7c4e1d7ad3f5e1c0b")

	got := s.ownBlindingKey(context.Background(), "sc-SEQ-USDX", script, nil)
	if len(got) != 33 {
		t.Fatalf("a transparent account got no blinding key for its own output (%x)", got)
	}
	buf, _ := hex.DecodeString(mbk)
	master, _ := slip77.FromMasterKey(buf)
	priv, pub, _ := master.DeriveKey(script)
	if !bytes.Equal(got, pub.SerializeCompressed()) {
		t.Fatalf("key %x is not the script's SLIP77 key %x", got, pub.SerializeCompressed())
	}
	// The private half the wallet unblinds that script's outputs with matches it.
	if !bytes.Equal(priv.PubKey().SerializeCompressed(), got) {
		t.Fatal("the wallet could not read an output blinded to this key")
	}
	// The account is found by namespace too, and each script has its own key.
	if k := s.ownBlindingKey(context.Background(), "bip84-account3", script, nil); !bytes.Equal(k, got) {
		t.Fatal("not found by namespace")
	}
	other, _ := hex.DecodeString("00140000000000000000000000000000000000000001")
	if bytes.Equal(s.ownBlindingKey(context.Background(), "sc-SEQ-USDX", other, nil), got) {
		t.Fatal("two scripts share a blinding key")
	}
}

func TestOwnBlindingKeyKeepsAConfidentialAddressKeyAndRefusesTheUnknown(t *testing.T) {
	s := &Service{wallet: &infoOnlyWallet{accounts: []ports.WalletAccount{
		walletAccount{label: "a", mbk: "zz"},
	}}}
	given := bytes.Repeat([]byte{2}, 33)
	if k := s.ownBlindingKey(context.Background(), "a", []byte{0}, given); !bytes.Equal(k, given) {
		t.Fatal("a confidential address's own key was replaced")
	}
	if k := s.ownBlindingKey(context.Background(), "a", []byte{0}, nil); k != nil {
		t.Fatal("a malformed master blinding key gave a key")
	}
	if k := s.ownBlindingKey(context.Background(), "missing", []byte{0}, nil); k != nil {
		t.Fatal("an unknown account gave a key: the swap must still be refused")
	}
}
