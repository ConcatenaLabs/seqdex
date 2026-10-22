package client

import (
	"errors"
	"strings"
	"testing"

	seqdexv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqdex/v1"
	"github.com/btcsuite/btcd/btcec/v2"
)

type refuseFinalizeWallet struct {
	Wallet
	finalized bool
}

func (w *refuseFinalizeWallet) ProposerFinalize(*seqdexv1.SwapAccept) (*seqdexv1.SwapComplete, string, error) {
	w.finalized = true
	return nil, "", errors.New("must not be called")
}

// A maker that cannot co-sign answers a sealed SwapFail; the taker stops at once with the
// reason and signs nothing. Before, the maker said nothing and the taker waited out its
// co-sign deadline (61 s on the web wallet).
func TestARefusedCoSignIsAnsweredAtOnce(t *testing.T) {
	mk, _ := btcec.NewPrivateKey()
	tk, _ := btcec.NewPrivateKey()
	makerC, _ := NewCrypter(mk, tk.PubKey())
	takerC, _ := NewCrypter(tk, mk.PubKey())
	for code, want := range map[uint32]string{
		FailCannotFund:   "the maker cannot fund this lift right now",
		FailNotTheOffer:  "the request does not match the offer",
		FailCannotCoSign: "the maker could not co-sign this lift",
		99:               "the maker could not co-sign this lift",
	} {
		sealed, err := SealSwapFail(makerC, "f1", code)
		if err != nil {
			t.Fatal(err)
		}
		w := &refuseFinalizeWallet{}
		_, _, err = (&Taker{Wallet: w}).Finalize(sealed, takerC)
		if !errors.Is(err, ErrMakerRefused) || !strings.HasSuffix(err.Error(), want) {
			t.Fatalf("code %d: got %v, want a refusal saying %q", code, err, want)
		}
		if w.finalized {
			t.Fatal("the taker signed after a refusal")
		}
	}
}

// A request that does not open under the session key is not answered: a refusal sealed
// under a substituted key could be read by whoever substituted it.
func TestARequestThatDoesNotOpenIsMarkedSo(t *testing.T) {
	mk, _ := btcec.NewPrivateKey()
	other, _ := btcec.NewPrivateKey()
	c, _ := NewCrypter(mk, other.PubKey())
	_, err := (&Maker{}).HandleRequest([]byte("not sealed for us at all, long enough to be read"), c)
	if !errors.Is(err, ErrOpenRequest) {
		t.Fatalf("got %v, want ErrOpenRequest", err)
	}
}
