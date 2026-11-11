package xchain

// namedlock.go builds swap version 2's Sequentia side: a lock that dies with
// its bitcoin payment. The specification is docs/cross-chain-swap-lock.md;
// this file holds its scripts and transactions, and the reading of a
// double-spend of the buyer's coin into the cancel's proof.
//
// The lock (P2WSH) has four branches:
//
//	claim       s, 1 CSV, the seller's Sc and the buyer's Bc (a claim fixed at swap time)
//	cancel      a Pb signature over a Bitcoin spend of C other than F, and the seller's Sx
//	late claim  s, k2 CSV, Bc alone
//	refund      CLTV T, Sx
//
// Its input is the holding output (2-of-2 Sh+Bh, or Sh after Th), confirmed
// before the bitcoin payment F. The cancel's witness gives a BIP143 preimage in
// pieces (the version, hashPrevouts||hashSequence cut at byte 25, and the tail
// from nSequence to the hash type); it refuses a preimage that begins with the
// signed-message prefix, and puts C's outpoint, Pb's P2WPKH
// script code and C's amount between them, hashes the 182 bytes once with
// SHA256, requires the result to differ from m0 (the same for F) and checks
// Pb's signature over it with OP_CHECKSIGFROMSTACKVERIFY, which hashes once
// more: exactly the hash Bitcoin signs for an input spending C. So only a
// second signed spend of C counts. Pb may be the key of a coin the buyer
// already holds, whatever else that key signs on either chain.

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/vulpemventures/go-elements/elementsutil"
	"github.com/vulpemventures/go-elements/transaction"
)

// SwapVersionNamedLock is the swap version both sides name for this lock.
const SwapVersionNamedLock = 2

// Opcodes Elements adds that btcd does not name.
const (
	opCat                     = 0x7e
	opCheckSigFromStackVerify = 0xc2
	cancelTailLen             = 44 // nSequence, hashOutputs, nLockTime, hash type
)

// SignedMessagePrefix is the first 25 bytes of every message Bitcoin and
// Sequentia sign with signmessage. A transaction's BIP143 preimage cannot begin
// with it past its 4 version bytes, since bytes 4-67 are hash output; the cancel
// refuses a preimage that does, so a signed message crafted to carry C's 70
// bytes at offset 68 is never a proof.
var SignedMessagePrefix = append([]byte{0x18}, []byte("Bitcoin Signed Message:\n")...)

// coinMiddle is the part of every BIP143 preimage spending C that the lock fixes:
// C's outpoint, Pb's P2WPKH script code and C's amount (70 bytes, at offset 68).
func coinMiddle(c wire.OutPoint, pb []byte, sats int64) []byte {
	b := make([]byte, 0, 70)
	b = append(b, c.Hash[:]...)
	b = binary.LittleEndian.AppendUint32(b, c.Index)
	b = append(b, 0x19, 0x76, 0xa9, 0x14)
	b = append(b, btcutil.Hash160(pb)...)
	b = append(b, 0x88, 0xac)
	return binary.LittleEndian.AppendUint64(b, uint64(sats))
}

// Sequence numbers of the lock's spends.
const (
	seqFinal       = 0xffffffff // the lock and the cancel: final, no replaceability signal
	seqRefund      = 0xfffffffe // the refund: lets CLTV act, does not signal
	seqClaimDelay  = 1          // the claim: one block after the lock (signals by itself)
	DefaultLateK2  = 5          // the late claim's delay, Sequentia blocks
	DefaultCoinAge = 3          // D: Bitcoin blocks the buyer's coin must sit below F
)

// NamedLockTerms are everything the lock's script commits to.
type NamedLockTerms struct {
	HashH []byte // SHA256(s)
	M0    []byte // SHA256(BIP143 preimage of F's input spending C)
	Pb    []byte // the key of the buyer's bitcoin coin C (33 bytes)
	C     wire.OutPoint
	CSats int64  // C's amount
	Bc    []byte // the buyer's claim key
	Sc    []byte // the seller's claim key (signs the fixed claim once)
	Sx    []byte // the seller's cancel and refund key
	T     uint32 // the refund's height
	K2    uint32 // the late claim's delay
}

func (t NamedLockTerms) check() error {
	if len(t.HashH) != 32 || len(t.M0) != 32 {
		return errors.New("named lock: hash and m0 must be 32 bytes")
	}
	for name, k := range map[string][]byte{"Pb": t.Pb, "Bc": t.Bc, "Sc": t.Sc, "Sx": t.Sx} {
		if len(k) != 33 {
			return fmt.Errorf("named lock: %s must be a 33-byte compressed key", name)
		}
	}
	if bytes.Equal(t.Sc, t.Sx) {
		// The fixed claim's Sc signature would then satisfy the cancel branch,
		// which the buyer can complete with its own Pb: a claim without the secret.
		return errors.New("named lock: the seller's claim and cancel keys must differ")
	}
	if t.CSats <= 0 {
		return errors.New("named lock: C's amount must be set")
	}
	if t.T == 0 || t.K2 == 0 || t.K2 > 0xffff {
		return errors.New("named lock: T and k2 must be set (k2 below 65536 blocks)")
	}
	return nil
}

// NamedLockScript returns the lock's witness script.
func NamedLockScript(t NamedLockTerms) ([]byte, error) {
	if err := t.check(); err != nil {
		return nil, err
	}
	b := txscript.NewScriptBuilder()
	b.AddOp(txscript.OP_IF) // claim
	b.AddOp(txscript.OP_SHA256).AddData(t.HashH).AddOp(txscript.OP_EQUALVERIFY)
	b.AddInt64(seqClaimDelay).AddOp(txscript.OP_CHECKSEQUENCEVERIFY).AddOp(txscript.OP_DROP)
	b.AddData(t.Sc).AddOp(txscript.OP_CHECKSIGVERIFY)
	b.AddData(t.Bc).AddOp(txscript.OP_CHECKSIG)
	b.AddOp(txscript.OP_ELSE)
	b.AddOp(txscript.OP_IF) // cancel: stack sig(Sx) sig(Pb) version midA midB tail
	b.AddOp(txscript.OP_SIZE).AddInt64(cancelTailLen).AddOp(txscript.OP_EQUALVERIFY).AddOp(txscript.OP_TOALTSTACK)
	b.AddOp(txscript.OP_SIZE).AddInt64(43).AddOp(txscript.OP_EQUALVERIFY).AddOp(txscript.OP_TOALTSTACK)
	b.AddOp(txscript.OP_SIZE).AddInt64(21).AddOp(txscript.OP_EQUALVERIFY)
	b.AddOp(txscript.OP_SWAP).AddOp(txscript.OP_SIZE).AddInt64(4).AddOp(txscript.OP_EQUALVERIFY).AddOp(txscript.OP_SWAP).AddOp(opCat)
	b.AddOp(txscript.OP_DUP).AddData(SignedMessagePrefix).AddOp(txscript.OP_EQUAL).AddOp(txscript.OP_NOT).AddOp(txscript.OP_VERIFY)
	b.AddOp(txscript.OP_FROMALTSTACK).AddOp(opCat)
	b.AddData(coinMiddle(t.C, t.Pb, t.CSats)).AddOp(opCat)
	b.AddOp(txscript.OP_FROMALTSTACK).AddOp(opCat)
	b.AddOp(txscript.OP_SHA256)
	b.AddOp(txscript.OP_DUP).AddData(t.M0).AddOp(txscript.OP_EQUAL).AddOp(txscript.OP_NOT).AddOp(txscript.OP_VERIFY)
	b.AddData(t.Pb).AddOp(opCheckSigFromStackVerify)
	b.AddData(t.Sx).AddOp(txscript.OP_CHECKSIG)
	b.AddOp(txscript.OP_ELSE)
	b.AddOp(txscript.OP_IF) // late claim
	b.AddOp(txscript.OP_SHA256).AddData(t.HashH).AddOp(txscript.OP_EQUALVERIFY)
	b.AddInt64(int64(t.K2)).AddOp(txscript.OP_CHECKSEQUENCEVERIFY).AddOp(txscript.OP_DROP)
	b.AddData(t.Bc).AddOp(txscript.OP_CHECKSIG)
	b.AddOp(txscript.OP_ELSE) // refund
	b.AddInt64(int64(t.T)).AddOp(txscript.OP_CHECKLOCKTIMEVERIFY).AddOp(txscript.OP_DROP)
	b.AddData(t.Sx).AddOp(txscript.OP_CHECKSIG)
	b.AddOp(txscript.OP_ENDIF).AddOp(txscript.OP_ENDIF).AddOp(txscript.OP_ENDIF)
	return b.Script()
}

// HoldingScript returns the holding output's witness script: 2-of-2 Sh+Bh, or
// Sh alone after Th.
func HoldingScript(sh, bh []byte, th uint32) ([]byte, error) {
	if len(sh) != 33 || len(bh) != 33 || bytes.Equal(sh, bh) || th == 0 {
		return nil, errors.New("holding output: two distinct 33-byte keys and Th are required")
	}
	b := txscript.NewScriptBuilder()
	b.AddOp(txscript.OP_IF)
	b.AddOp(txscript.OP_2).AddData(sh).AddData(bh).AddOp(txscript.OP_2).AddOp(txscript.OP_CHECKMULTISIG)
	b.AddOp(txscript.OP_ELSE)
	b.AddInt64(int64(th)).AddOp(txscript.OP_CHECKLOCKTIMEVERIFY).AddOp(txscript.OP_DROP)
	b.AddData(sh).AddOp(txscript.OP_CHECKSIG)
	b.AddOp(txscript.OP_ENDIF)
	return b.Script()
}

// ClaimOutScript is the fixed claim's payment output: the buyer's key, one
// block after the claim, so no child can pin the claim in the mempool.
func ClaimOutScript(bo []byte) ([]byte, error) {
	if len(bo) != 33 {
		return nil, errors.New("claim output: a 33-byte key is required")
	}
	b := txscript.NewScriptBuilder()
	b.AddData(bo).AddOp(txscript.OP_CHECKSIGVERIFY).AddInt64(1).AddOp(txscript.OP_CHECKSEQUENCEVERIFY)
	return b.Script()
}

// P2WSH returns the v0 witness scriptPubKey of a witness script.
func P2WSH(ws []byte) []byte {
	h := sha256.Sum256(ws)
	return append([]byte{0x00, 0x20}, h[:]...)
}

// --- Sequentia transactions ----------------------------------------------------

// NamedSpend names one explicit output to spend.
type NamedSpend struct {
	TxID    string
	Vout    uint32
	Amount  uint64
	AssetID string
}

// buildOneInOneOut builds a version-2 transaction spending one explicit output
// into one payment and one explicit fee output, both in the input's asset.
func buildOneInOneOut(in NamedSpend, destSPK []byte, fee uint64, sequence, locktime uint32) (*transaction.Transaction, error) {
	if fee == 0 || fee >= in.Amount {
		return nil, fmt.Errorf("named lock: fee %d must be positive and below the amount %d", fee, in.Amount)
	}
	prev, err := elementsutil.TxIDToBytes(in.TxID)
	if err != nil {
		return nil, err
	}
	asset, err := elementsutil.AssetHashToBytes(in.AssetID)
	if err != nil {
		return nil, err
	}
	pay, err := elementsutil.ValueToBytes(in.Amount - fee)
	if err != nil {
		return nil, err
	}
	fv, err := elementsutil.ValueToBytes(fee)
	if err != nil {
		return nil, err
	}
	tx := transaction.NewTx(2)
	ti := transaction.NewTxInput(prev, in.Vout)
	ti.Sequence = sequence
	tx.AddInput(ti)
	tx.Locktime = locktime
	tx.AddOutput(transaction.NewTxOutput(asset, pay, destSPK))
	tx.AddOutput(transaction.NewTxOutput(asset, fv, []byte{}))
	return tx, nil
}

// BuildLockTx builds L: the holding output into the lock, the rest as fee.
func BuildLockTx(hold NamedSpend, lockScript []byte, lockAmount uint64) (*transaction.Transaction, error) {
	if lockAmount >= hold.Amount {
		return nil, fmt.Errorf("named lock: the holding output %d must exceed the lock %d by its fee", hold.Amount, lockAmount)
	}
	return buildOneInOneOut(hold, P2WSH(lockScript), hold.Amount-lockAmount, seqFinal, 0)
}

// BuildClaimTx builds the fixed claim: the lock into the claim output.
func BuildClaimTx(lock NamedSpend, bo []byte, fee uint64) (*transaction.Transaction, error) {
	cs, err := ClaimOutScript(bo)
	if err != nil {
		return nil, err
	}
	return buildOneInOneOut(lock, P2WSH(cs), fee, seqClaimDelay, 0)
}

// BuildCancelTx builds the cancel paying the seller's script.
func BuildCancelTx(lock NamedSpend, destSPK []byte, fee uint64) (*transaction.Transaction, error) {
	return buildOneInOneOut(lock, destSPK, fee, seqFinal, 0)
}

// BuildRefundTx builds the refund, valid from height T.
func BuildRefundTx(lock NamedSpend, destSPK []byte, fee uint64, t uint32) (*transaction.Transaction, error) {
	return buildOneInOneOut(lock, destSPK, fee, seqRefund, t)
}

// BuildLateClaimTx builds the buyer's lone claim, valid k2 blocks after the lock.
func BuildLateClaimTx(lock NamedSpend, destSPK []byte, fee uint64, k2 uint32) (*transaction.Transaction, error) {
	return buildOneInOneOut(lock, destSPK, fee, k2, 0)
}

// BuildClaimOutSpend moves the claimed asset from the claim output to the buyer's address.
func BuildClaimOutSpend(out NamedSpend, destSPK []byte, fee uint64) (*transaction.Transaction, error) {
	return buildOneInOneOut(out, destSPK, fee, 1, 0)
}

// WitnessSighash is the SIGHASH_ALL hash of input 0 of tx, spending an explicit
// output of `amount` under witness script ws.
func WitnessSighash(tx *transaction.Transaction, ws []byte, amount uint64) ([32]byte, error) {
	v, err := elementsutil.ValueToBytes(amount)
	if err != nil {
		return [32]byte{}, err
	}
	return tx.HashForWitnessV0(0, ws, v, txscript.SigHashAll), nil
}

// SignWitness signs input 0 of tx with key, SIGHASH_ALL; DER || 0x01.
func SignWitness(tx *transaction.Transaction, ws []byte, amount uint64, key *Key) ([]byte, error) {
	h, err := WitnessSighash(tx, ws, amount)
	if err != nil {
		return nil, err
	}
	return append(key.SignDER(h[:]), byte(txscript.SigHashAll)), nil
}

// VerifyWitnessSig checks a DER || SIGHASH_ALL signature by pub over input 0 of tx.
func VerifyWitnessSig(tx *transaction.Transaction, ws []byte, amount uint64, pub, sig []byte) error {
	if len(sig) < 9 || sig[len(sig)-1] != byte(txscript.SigHashAll) {
		return errors.New("signature is not SIGHASH_ALL")
	}
	h, err := WitnessSighash(tx, ws, amount)
	if err != nil {
		return err
	}
	s, err := ecdsa.ParseDERSignature(sig[:len(sig)-1])
	if err != nil {
		return err
	}
	pk, err := btcec.ParsePubKey(pub)
	if err != nil {
		return err
	}
	if !s.Verify(h[:], pk) {
		return errors.New("signature does not verify")
	}
	return nil
}

func setWitness(tx *transaction.Transaction, items ...[]byte) (string, error) {
	w := make(transaction.TxWitness, len(items))
	copy(w, items)
	tx.Inputs[0].Witness = w
	return tx.ToHex()
}

// FinalizeLockTx completes L with both holding signatures (Sh first, as in the script).
func FinalizeLockTx(tx *transaction.Transaction, holding, sigSh, sigBh []byte) (string, error) {
	return setWitness(tx, []byte{}, sigSh, sigBh, []byte{0x01}, holding)
}

// FinalizeClaimTx completes the fixed claim.
func FinalizeClaimTx(tx *transaction.Transaction, lock, sigBc, sigSc, secret []byte) (string, error) {
	return setWitness(tx, sigBc, sigSc, secret, []byte{0x01}, lock)
}

// CancelProofPieces is what the cancel's witness carries: Pb's DER signature (no
// hash-type byte) and the BIP143 preimage of a spend of C, less the part the lock fixes.
type CancelProofPieces struct {
	Sig     []byte
	Version []byte // bytes 0-3
	MidA    []byte // bytes 4-24
	MidB    []byte // bytes 25-67
	Tail    []byte // bytes 138-181: nSequence || hashOutputs || nLockTime || hash type
}

// PiecesOf cuts a 182-byte BIP143 preimage as the cancel's witness carries it.
func PiecesOf(sig, pre []byte) CancelProofPieces {
	return CancelProofPieces{Sig: sig, Version: pre[0:4], MidA: pre[4:25], MidB: pre[25:68], Tail: pre[138:182]}
}

// FinalizeCancelTx completes the cancel with a proof.
func FinalizeCancelTx(tx *transaction.Transaction, lock, sigSx []byte, p CancelProofPieces) (string, error) {
	return setWitness(tx, sigSx, p.Sig, p.Version, p.MidA, p.MidB, p.Tail, []byte{0x01}, []byte{}, lock)
}

// FinalizeLateClaimTx completes the buyer's lone claim.
func FinalizeLateClaimTx(tx *transaction.Transaction, lock, sigBc, secret []byte) (string, error) {
	return setWitness(tx, sigBc, secret, []byte{0x01}, []byte{}, []byte{}, lock)
}

// FinalizeRefundTx completes the refund.
func FinalizeRefundTx(tx *transaction.Transaction, lock, sigSx []byte) (string, error) {
	return setWitness(tx, sigSx, []byte{}, []byte{}, []byte{}, lock)
}

// FinalizeClaimOutSpend completes the move of a claim output.
func FinalizeClaimOutSpend(tx *transaction.Transaction, claimOut, sigBo []byte) (string, error) {
	return setWitness(tx, sigBo, claimOut)
}

// --- the bitcoin payment and its proof ----------------------------------------------

func dsha(b []byte) []byte { h := chainhash.DoubleHashB(b); return h }

// BIP143Message returns SHA256 of the BIP143 preimage of input idx of tx,
// spending a P2WPKH output of `amount` sats under pub, for hash type ht. Its
// SHA256 is the hash Bitcoin signs, and the message the lock's cancel reads.
func BIP143Message(tx *wire.MsgTx, idx int, pub []byte, amount int64, ht txscript.SigHashType) ([]byte, error) {
	pre, err := BIP143Preimage(tx, idx, pub, amount, ht)
	if err != nil {
		return nil, err
	}
	m := sha256.Sum256(pre)
	return m[:], nil
}

// BIP143Preimage is the BIP143 preimage of input idx of tx spending a P2WPKH
// output of `amount` sats under pub, for hash type ht (182 bytes).
func BIP143Preimage(tx *wire.MsgTx, idx int, pub []byte, amount int64, ht txscript.SigHashType) ([]byte, error) {
	if idx < 0 || idx >= len(tx.TxIn) {
		return nil, errors.New("input index out of range")
	}
	base := ht & 0x1f
	acp := ht&txscript.SigHashAnyOneCanPay != 0
	zero := make([]byte, 32)
	var buf bytes.Buffer
	le32 := func(v uint32) { var b [4]byte; binary.LittleEndian.PutUint32(b[:], v); buf.Write(b[:]) }
	le32(uint32(tx.Version))
	if acp {
		buf.Write(zero)
	} else {
		var p bytes.Buffer
		for _, in := range tx.TxIn {
			p.Write(in.PreviousOutPoint.Hash[:])
			var b [4]byte
			binary.LittleEndian.PutUint32(b[:], in.PreviousOutPoint.Index)
			p.Write(b[:])
		}
		buf.Write(dsha(p.Bytes()))
	}
	if acp || base == txscript.SigHashSingle || base == txscript.SigHashNone {
		buf.Write(zero)
	} else {
		var s bytes.Buffer
		for _, in := range tx.TxIn {
			var b [4]byte
			binary.LittleEndian.PutUint32(b[:], in.Sequence)
			s.Write(b[:])
		}
		buf.Write(dsha(s.Bytes()))
	}
	in := tx.TxIn[idx]
	buf.Write(in.PreviousOutPoint.Hash[:])
	le32(in.PreviousOutPoint.Index)
	buf.Write(append([]byte{0x19, 0x76, 0xa9, 0x14}, append(btcutil.Hash160(pub), 0x88, 0xac)...))
	var a [8]byte
	binary.LittleEndian.PutUint64(a[:], uint64(amount))
	buf.Write(a[:])
	le32(in.Sequence)
	switch {
	case base != txscript.SigHashSingle && base != txscript.SigHashNone:
		var o bytes.Buffer
		for _, out := range tx.TxOut {
			if err := wire.WriteTxOut(&o, 0, 0, out); err != nil {
				return nil, err
			}
		}
		buf.Write(dsha(o.Bytes()))
	case base == txscript.SigHashSingle && idx < len(tx.TxOut):
		var o bytes.Buffer
		if err := wire.WriteTxOut(&o, 0, 0, tx.TxOut[idx]); err != nil {
			return nil, err
		}
		buf.Write(dsha(o.Bytes()))
	default:
		buf.Write(zero)
	}
	le32(tx.LockTime)
	le32(uint32(ht))
	return buf.Bytes(), nil
}

// PaymentM0 is m0 for the agreed payment F (unsigned is enough: segwit's
// message does not cover the witness). F must spend exactly one input, C.
func PaymentM0(f *wire.MsgTx, pb []byte, coinSats int64) ([]byte, error) {
	if len(f.TxIn) != 1 {
		return nil, fmt.Errorf("the bitcoin payment must spend exactly one coin, has %d inputs", len(f.TxIn))
	}
	return BIP143Message(f, 0, pb, coinSats, txscript.SigHashAll)
}

// ParseBTCTx decodes a Bitcoin transaction from hex.
func ParseBTCTx(rawHex string) (*wire.MsgTx, error) {
	raw, err := hex.DecodeString(rawHex)
	if err != nil {
		return nil, err
	}
	tx := wire.NewMsgTx(2)
	if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
		return nil, err
	}
	return tx, nil
}

// ErrNoCancelProof means the spend of C carries no Pb signature over a message
// other than m0: it is F itself, or not a P2WPKH spend by Pb.
var ErrNoCancelProof = errors.New("no cancel proof in this spend")

// CancelProof reads a Bitcoin transaction that spends the buyer's coin C and
// returns what the lock's cancel needs. It refuses F's own signature (message
// m0) and a spend that is not a P2WPKH spend by Pb.
func CancelProof(spender *wire.MsgTx, c wire.OutPoint, pb []byte, coinSats int64, m0 []byte) (CancelProofPieces, error) {
	var p CancelProofPieces
	idx := -1
	for i, in := range spender.TxIn {
		if in.PreviousOutPoint == c {
			idx = i
			break
		}
	}
	if idx < 0 {
		return p, fmt.Errorf("%w: it does not spend %s", ErrNoCancelProof, c)
	}
	w := spender.TxIn[idx].Witness
	if len(w) != 2 || !bytes.Equal(w[1], pb) || len(w[0]) < 9 {
		return p, fmt.Errorf("%w: input %d is not a P2WPKH spend by Pb", ErrNoCancelProof, idx)
	}
	ht := txscript.SigHashType(w[0][len(w[0])-1])
	pre, err := BIP143Preimage(spender, idx, pb, coinSats, ht)
	if err != nil {
		return p, err
	}
	data := sha256.Sum256(pre)
	if bytes.Equal(data[:], m0) {
		return p, fmt.Errorf("%w: it is the agreed payment's own signature", ErrNoCancelProof)
	}
	s, err := ecdsa.ParseDERSignature(w[0][:len(w[0])-1])
	if err != nil {
		return p, fmt.Errorf("%w: %v", ErrNoCancelProof, err)
	}
	pk, err := btcec.ParsePubKey(pb)
	if err != nil {
		return p, err
	}
	h := sha256.Sum256(data[:])
	if !s.Verify(h[:], pk) {
		return p, fmt.Errorf("%w: the signature does not verify", ErrNoCancelProof)
	}
	return PiecesOf(lowS(w[0][:len(w[0])-1]), pre), nil
}

// lowS rewrites a DER signature with S above half the order as its low-S twin,
// which Sequentia's relay policy requires and which signs the same message.
func lowS(der []byte) []byte {
	// DER: 30 len 02 rlen r 02 slen s
	if len(der) < 8 || der[0] != 0x30 || der[2] != 0x02 {
		return der
	}
	rlen := int(der[3])
	if 4+rlen+2 > len(der) || der[4+rlen] != 0x02 {
		return der
	}
	r := der[4 : 4+rlen]
	s := new(big.Int).SetBytes(der[6+rlen:])
	n := btcec.S256().N
	half := new(big.Int).Rsh(n, 1)
	if s.Cmp(half) <= 0 {
		return der
	}
	s.Sub(n, s)
	sb := s.Bytes()
	if sb[0]&0x80 != 0 {
		sb = append([]byte{0}, sb...)
	}
	out := []byte{0x30, byte(4 + len(r) + len(sb)), 0x02, byte(len(r))}
	out = append(out, r...)
	out = append(out, 0x02, byte(len(sb)))
	return append(out, sb...)
}

// P2WPKHScript is the scriptPubKey of a P2WPKH output under pub.
func P2WPKHScript(pub []byte) []byte {
	return append([]byte{0x00, 0x14}, btcutil.Hash160(pub)...)
}

// CheckCoinDepth refuses a buyer's coin confirmed fewer than d Bitcoin blocks
// below the payment: a reorganization below C's own block would let the buyer
// spend C's parent instead, which carries no Pb signature.
func CheckCoinDepth(cHeight, fHeight int64, d int) error {
	if cHeight <= 0 || fHeight <= 0 {
		return errors.New("the coin and the payment must both be confirmed")
	}
	if fHeight-cHeight < int64(d) {
		return fmt.Errorf("the buyer's coin is %d block(s) below the payment, the seller covers %d", fHeight-cHeight, d)
	}
	return nil
}
