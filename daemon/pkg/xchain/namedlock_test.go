package xchain

import (
	"bytes"
	"crypto/sha256"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// The message the lock's cancel reads, hashed once more, is the hash Bitcoin
// signs for every hash type (btcd's own BIP143 implementation is the reference).
func TestBIP143MessageMatchesBitcoinSighash(t *testing.T) {
	k, _ := NewKey()
	pk := k.PubKey()
	tx := wire.NewMsgTx(2)
	for i := 0; i < 3; i++ {
		tx.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Hash: chainhash.Hash{byte(i + 1)}, Index: uint32(i)}, Sequence: 0xfffffffd - uint32(i)})
		tx.AddTxOut(&wire.TxOut{Value: int64(1000 * (i + 1)), PkScript: P2WPKHScript(pk)})
	}
	tx.LockTime = 77
	fetch := txscript.NewCannedPrevOutputFetcher(P2WPKHScript(pk), 50_000)
	hashes := txscript.NewTxSigHashes(tx, fetch)
	for _, ht := range []txscript.SigHashType{txscript.SigHashAll, txscript.SigHashNone, txscript.SigHashSingle,
		txscript.SigHashAll | txscript.SigHashAnyOneCanPay, txscript.SigHashSingle | txscript.SigHashAnyOneCanPay} {
		for idx := 0; idx < 3; idx++ {
			want, err := txscript.CalcWitnessSigHash(P2WPKHScript(pk), hashes, ht, tx, idx, 50_000)
			if err != nil {
				t.Fatal(err)
			}
			m, err := BIP143Message(tx, idx, pk, 50_000, ht)
			if err != nil {
				t.Fatal(err)
			}
			got := sha256.Sum256(m)
			if !bytes.Equal(got[:], want) {
				t.Fatalf("hash type %d input %d: %x != %x", ht, idx, got, want)
			}
		}
	}
}

func TestLowSKeepsTheMessage(t *testing.T) {
	k, _ := NewKey()
	h := sha256.Sum256([]byte("m"))
	der := k.SignDER(h[:])
	s, _ := ecdsa.ParseDERSignature(der)
	_ = s
	// Build the high-S twin by hand and check lowS brings it back and it verifies.
	rlen := int(der[3])
	r := der[4 : 4+rlen]
	sv := new(btcec.ModNScalar)
	sv.SetByteSlice(der[6+rlen:])
	sv.Negate()
	sb := sv.Bytes()
	hs := sb[:]
	if hs[0]&0x80 != 0 {
		hs = append([]byte{0}, hs...)
	}
	high := append([]byte{0x30, byte(4 + len(r) + len(hs)), 0x02, byte(len(r))}, r...)
	high = append(high, 0x02, byte(len(hs)))
	high = append(high, hs...)
	low := lowS(high)
	if !bytes.Equal(low, der) {
		t.Fatalf("lowS(high) = %x, want %x", low, der)
	}
}

func TestNamedLockRefusesOneSellerKeyForClaimAndCancel(t *testing.T) {
	a, _ := NewKey()
	b, _ := NewKey()
	h := make([]byte, 32)
	_, err := NamedLockScript(NamedLockTerms{HashH: h, M0: h, Pb: b.PubKey(), Bc: b.PubKey(), Sc: a.PubKey(), Sx: a.PubKey(), T: 10, K2: 5})
	if err == nil {
		t.Fatal("a lock whose claim and cancel share the seller's key was built")
	}
}
