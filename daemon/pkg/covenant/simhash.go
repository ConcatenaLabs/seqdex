package covenant

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"

	"github.com/vulpemventures/go-elements/transaction"
)

// Simplicity's transaction hashes, as the chain's interpreter computes them
// (the node's src/simplicity/elements/env.c and txEnv.c), so that this package
// can sign what a program checks with no compiler:
//
//   - SimTxHash is jet::tx_hash(): the version, lock time, every input's
//     outpoint, sequence and annex, every spent coin's asset, amount and
//     script, every output and the issuances;
//   - SimSigAllHash is jet::sig_all_hash() for one input: the genesis hash
//     twice, tx_hash, the input's taproot leaf and path, and its index;
//   - ClearingDigest is what a seqdex/round-order's clearer signs.
//
// Each refuses an input that is a peg-in or carries an issuance, which no
// transaction this package builds holds. The genesis hash is in internal byte
// order, the reverse of the hex an RPC prints.

// SpentCoin is a coin an input spends, as the transaction's environment reads
// it: its asset and amount in their consensus encoding (explicit: 0x01 and 32
// or 8 bytes) and its scriptPubKey.
type SpentCoin struct {
	Asset  []byte
	Value  []byte
	Script []byte
}

// ExplicitCoin is the SpentCoin of an explicit coin: asset in internal byte order.
func ExplicitCoin(asset [32]byte, value uint64, script []byte) SpentCoin {
	v := make([]byte, 9)
	v[0] = 0x01
	binary.BigEndian.PutUint64(v[1:], value)
	return SpentCoin{Asset: append([]byte{0x01}, asset[:]...), Value: v, Script: append([]byte{}, script...)}
}

func sum(parts ...[]byte) []byte {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

// conf is a confidential field as Simplicity hashes it: a null field is the byte
// 0x00, any other its consensus encoding.
func conf(b []byte) []byte {
	if len(b) == 0 || (len(b) == 1 && b[0] == 0x00) {
		return []byte{0x00}
	}
	return b
}

// annexOf is a witness stack's annex as the node gives it to Simplicity: the
// last item without its 0x50 tag, when the stack holds at least two items and
// that item starts with the tag.
func annexOf(w [][]byte) ([]byte, bool) {
	if len(w) >= 2 && len(w[len(w)-1]) > 0 && w[len(w)-1][0] == 0x50 {
		return w[len(w)-1][1:], true
	}
	return nil, false
}

func be32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

// SimTxHash is Simplicity's jet::tx_hash() of tx spending coins (one per input).
func SimTxHash(tx *transaction.Transaction, coins []SpentCoin) ([32]byte, error) {
	var out [32]byte
	if len(coins) != len(tx.Inputs) {
		return out, fmt.Errorf("%d spent coins for %d inputs", len(coins), len(tx.Inputs))
	}
	empty := sum()
	var outpoints, sequences, annexes, inAmounts, inScripts []byte
	var issAssets, issTokens, issProofs, issEntropy []byte
	for i, in := range tx.Inputs {
		if in.IsPegin {
			return out, fmt.Errorf("input %d is a peg-in", i)
		}
		if in.Issuance != nil {
			return out, fmt.Errorf("input %d carries an issuance", i)
		}
		if len(in.Hash) != 32 {
			return out, fmt.Errorf("input %d: previous txid of %d bytes", i, len(in.Hash))
		}
		outpoints = append(outpoints, 0x00)
		outpoints = append(outpoints, in.Hash...)
		outpoints = append(outpoints, be32(in.Index)...)
		sequences = append(sequences, be32(in.Sequence)...)
		if a, ok := annexOf(in.Witness); ok {
			annexes = append(annexes, 0x01)
			annexes = append(annexes, sum(a)...)
		} else {
			annexes = append(annexes, 0x00)
		}
		c := coins[i]
		inAmounts = append(inAmounts, conf(c.Asset)...)
		inAmounts = append(inAmounts, conf(c.Value)...)
		inScripts = append(inScripts, sum(c.Script)...)
		issAssets = append(issAssets, 0, 0)
		issTokens = append(issTokens, 0, 0)
		issProofs = append(issProofs, empty...)
		issProofs = append(issProofs, empty...)
		issEntropy = append(issEntropy, 0x00)
	}
	var outAmounts, outNonces, outScripts, outRanges, outSurj []byte
	for _, o := range tx.Outputs {
		outAmounts = append(outAmounts, conf(o.Asset)...)
		outAmounts = append(outAmounts, conf(o.Value)...)
		outNonces = append(outNonces, conf(o.Nonce)...)
		outScripts = append(outScripts, sum(o.Script)...)
		rp, sp := empty, empty
		if len(o.Value) > 0 && (o.Value[0] == 0x08 || o.Value[0] == 0x09) {
			rp = sum(o.RangeProof)
		}
		if len(o.Asset) > 0 && (o.Asset[0] == 0x0a || o.Asset[0] == 0x0b) {
			sp = sum(o.SurjectionProof)
		}
		outRanges = append(outRanges, rp...)
		outSurj = append(outSurj, sp...)
	}
	inputs := sum(sum(outpoints), sum(sequences), sum(annexes))
	utxos := sum(sum(inAmounts), sum(inScripts))
	issuances := sum(sum(issAssets), sum(issTokens), sum(issProofs), sum(issEntropy))
	outputs := sum(sum(outAmounts), sum(outNonces), sum(outScripts), sum(outRanges))
	copy(out[:], sum(be32(uint32(tx.Version)), be32(tx.Locktime), inputs, outputs, issuances, sum(outSurj), utxos))
	return out, nil
}

// SimSigAllHash is jet::sig_all_hash() for the Simplicity program whose root is
// cmr, spent at input index under controlBlock: SHA256(genesis || genesis ||
// tx_hash || tap_env_hash || index), tap_env_hash being SHA256 of the leaf
// hash, the hash of the control block's path and its internal key.
func SimSigAllHash(genesis [32]byte, tx *transaction.Transaction, coins []SpentCoin, index uint32, cmr [32]byte, controlBlock []byte) ([32]byte, error) {
	var out [32]byte
	if len(controlBlock) < 33 || (len(controlBlock)-33)%32 != 0 {
		return out, fmt.Errorf("control block of %d bytes", len(controlBlock))
	}
	th, err := SimTxHash(tx, coins)
	if err != nil {
		return out, err
	}
	leaf := taggedHash("TapLeaf/elements", []byte{controlBlock[0] & 0xfe, 0x20}, cmr[:])
	path := sum(controlBlock[33:])
	env := sum(leaf[:], path, controlBlock[1:33])
	copy(out[:], sum(genesis[:], genesis[:], th[:], env, be32(index)))
	return out, nil
}

// ClearingTag is the tag of the digest a round order's clearer signs.
const ClearingTag = "SeqDEX/clearing"

// ClearingDigest is what a seqdex/round-order's CLEARER signs (BIP340) to make
// tx a round: SHA256(T || T || genesis || tx_hash), T = SHA256("SeqDEX/clearing").
// Witnesses are outside it, so the clearer signs once the transaction is final
// and the one signature serves every round order of that clearer in it.
func ClearingDigest(genesis [32]byte, tx *transaction.Transaction, coins []SpentCoin) ([32]byte, error) {
	var out [32]byte
	th, err := SimTxHash(tx, coins)
	if err != nil {
		return out, err
	}
	t := sum([]byte(ClearingTag))
	copy(out[:], sum(t, t, genesis[:], th[:]))
	return out, nil
}
