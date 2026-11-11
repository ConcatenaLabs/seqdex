package restricted

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
)

// Every Sequentia transaction a party makes in a cross-chain swap, the lock,
// the claim and the refunds, signals no replaceability (BIP125): the claim
// carries the secret, and a lock that could be replaced would take the
// buyer's claim, its child, out of a mempool with it after an anchor reorg,
// once the secret is public. The lock and the claim are final; a refund
// carries its lock time T_seq with sequences one below final, which CLTV needs
// and which still signal nothing.
func TestSwapTransactionsAreNotReplaceable(t *testing.T) {
	key := func() [32]byte {
		k, err := btcec.NewPrivateKey()
		if err != nil {
			t.Fatal(err)
		}
		return XOnly(k)
	}
	s := Swap{Hash: [32]byte{1}, BuyerX: key(), PolicyX: key(), TSeq: 1_800_000_000, SellerEnclave: key(),
		Asset: [32]byte{0xaa}, Value: 300, PotAsset: [32]byte{0xff}, PotValue: 5000, SellerPayout: key()}
	htlc, pot, err := s.Trees()
	if err != nil {
		t.Fatal(err)
	}
	enc := Coin{Txid: swapTid(1), Asset: s.Asset, Value: 400, Script: append([]byte{0x51, 0x20}, s.SellerEnclave[:]...)}
	fee := Coin{Txid: swapTid(2), Asset: s.PotAsset, Value: 9000, Script: []byte{0x00, 0x14, 2}}
	lock, err := BuildLock(LockPlan{Swap: s, EnclaveCoins: []Coin{enc}, FeeCoins: []Coin{fee}, FeeChange: []byte{0x00, 0x14, 3}, Fee: 300})
	if err != nil {
		t.Fatal(err)
	}
	hc := Coin{Txid: swapTid(3), Asset: s.Asset, Value: s.Value, Script: htlc.ScriptPubKey}
	pc := Coin{Txid: swapTid(3), Vout: 1, Asset: s.PotAsset, Value: s.PotValue, Script: pot.ScriptPubKey}
	claim, err := BuildClaim(ClaimPlan{Swap: s, HTLC: hc, Pot: pc, BuyerEnclave: []byte{0x51, 0x20, 4}, PotChange: []byte{0x00, 0x14, 5}, Fee: 300})
	if err != nil {
		t.Fatal(err)
	}
	refund, err := BuildRefund(RefundPlan{Coin: hc, Home: enc.Script, Leaf: htlc.Leaves["refund"], FeeCoin: fee, TSeq: s.TSeq})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name     string
		u        *Unsigned
		locktime uint32
		seq      uint32
	}{{"lock", lock, 0, 0xffffffff}, {"claim", claim, 0, 0xffffffff}, {"refund", refund, s.TSeq, 0xfffffffe}} {
		if c.u.Tx.Locktime != c.locktime {
			t.Errorf("the %s has lock time %d, want %d", c.name, c.u.Tx.Locktime, c.locktime)
		}
		for i, in := range c.u.Tx.Inputs {
			if in.Sequence != c.seq || in.Sequence < 0xfffffffe {
				t.Errorf("the %s's input %d has sequence %#x, want %#x (no BIP125 signal)", c.name, i, in.Sequence, c.seq)
			}
		}
	}
}

func swapTid(n byte) string {
	b := make([]byte, 64)
	for i := range b {
		b[i] = '0'
	}
	b[63] = '0' + n
	return string(b)
}
