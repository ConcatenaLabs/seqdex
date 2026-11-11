package restricted

// Delivery against payment: a restricted asset A for another asset B in one
// transaction, judged by A's policy server before anyone signs (POST
// /v1/check) and broadcast by it once both have (POST /v1/cosign with
// "broadcast": true), so its co-sign never exists apart from a broadcast
// (openamp README, "Trades"). The seller of A signs its enclave inputs and an
// expiry after which the server refuses the trade; the buyer signs its own
// coins, SIGHASH_ALL. Nothing is valid until the last signature exists.

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chaincfg/chainhash"

	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
)

// DvP lays out a trade. Outputs, in order: 0 the asset to the buyer's
// enclave; 1 the payment to the seller; the seller's change of A home to its
// enclave; the buyer's change of B; the fee payer's change; the fee.
type DvP struct {
	Asset         [32]byte // A, internal order
	Value         uint64
	AssetB        [32]byte // B, internal order
	Price         uint64   // B paid for Value
	SellerEnclave []byte   // A's change goes home here
	BuyerEnclave  []byte   // A is delivered here
	SellerRecv    []byte   // where B is paid
	SellerCoins   []Coin   // A, at the seller's enclave
	BuyerCoins    []Coin   // B
	BuyerChange   []byte
	FeeCoins      []Coin // the fee payer's, in FeeAsset
	FeeAsset      [32]byte
	Fee           uint64
	FeeChange     []byte
}

// SellerInputs are the indices of the seller's enclave inputs: the first.
func (d DvP) SellerInputs() []int {
	ins := make([]int, len(d.SellerCoins))
	for i := range ins {
		ins[i] = i
	}
	return ins
}

func sumOf(cs []Coin, asset [32]byte) (uint64, error) {
	var s uint64
	for _, c := range cs {
		if c.Asset != asset {
			return 0, fmt.Errorf("coin %s:%d is not the expected asset", c.Txid, c.Vout)
		}
		s += c.Value
	}
	return s, nil
}

// BuildDvP lays the trade out.
func BuildDvP(d DvP) (*Unsigned, error) {
	if d.FeeAsset == d.Asset {
		return nil, fmt.Errorf("the fee may not be paid in the restricted asset")
	}
	a, err := sumOf(d.SellerCoins, d.Asset)
	if err != nil {
		return nil, err
	}
	b, err := sumOf(d.BuyerCoins, d.AssetB)
	if err != nil {
		return nil, err
	}
	f, err := sumOf(d.FeeCoins, d.FeeAsset)
	if err != nil {
		return nil, err
	}
	if a < d.Value || b < d.Price || f < d.Fee {
		return nil, fmt.Errorf("the coins hold %d A of %d, %d B of %d, %d of the fee %d", a, d.Value, b, d.Price, f, d.Fee)
	}
	outs := []Out{{d.Asset, d.Value, d.BuyerEnclave}, {d.AssetB, d.Price, d.SellerRecv}}
	if a > d.Value {
		outs = append(outs, Out{d.Asset, a - d.Value, d.SellerEnclave})
	}
	if b > d.Price {
		outs = append(outs, Out{d.AssetB, b - d.Price, d.BuyerChange})
	}
	if f > d.Fee {
		outs = append(outs, Out{d.FeeAsset, f - d.Fee, d.FeeChange})
	}
	outs = append(outs, Out{d.FeeAsset, d.Fee, nil})
	coins := append(append(append([]Coin{}, d.SellerCoins...), d.BuyerCoins...), d.FeeCoins...)
	return Build(coins, outs, 0)
}

// CheckDvP is what a side checks of a trade before it signs, from the
// transaction itself. mine are the coins it offered; walletOwns are every
// outpoint its wallet would sign, none of which may be spent unless offered
// (a wallet signs every input it owns). ours are the scripts it controls: what
// the trade pays to them comes back to it. Per asset, it gives at most
// maxGive (what it offered beyond that must come back), and it receives each
// of gets.
func CheckDvP(u *Unsigned, mine []Coin, walletOwns map[string]bool, ours [][]byte, maxGive map[[32]byte]uint64, gets []Out) error {
	offered := map[string]bool{}
	in := map[[32]byte]uint64{}
	for _, c := range mine {
		offered[outpointKey(c.Txid, c.Vout)] = true
		in[c.Asset] += c.Value
	}
	for _, ti := range u.Tx.Inputs {
		h, err := chainhash.NewHash(ti.Hash)
		if err != nil {
			return err
		}
		k := outpointKey(h.String(), ti.Index)
		if walletOwns[k] && !offered[k] {
			return fmt.Errorf("the trade spends %s, a coin of ours not offered", k)
		}
		delete(offered, k)
	}
	if len(offered) != 0 {
		return fmt.Errorf("the trade leaves out coins we offered")
	}
	isOurs := func(spk []byte) bool {
		for _, o := range ours {
			if bytes.Equal(o, spk) {
				return true
			}
		}
		return false
	}
	back := map[[32]byte]uint64{}
	for _, o := range u.Tx.Outputs {
		v, ok := explicitValue(o.Value)
		if !ok || len(o.Asset) != 33 || o.Asset[0] != 1 {
			return fmt.Errorf("the trade carries a confidential output")
		}
		var a [32]byte
		copy(a[:], o.Asset[1:])
		if isOurs(o.Script) {
			back[a] += v
		}
	}
	for a, v := range in {
		if back[a] < v && v-back[a] > maxGive[a] {
			return fmt.Errorf("the trade takes %d of %s from us, more than the %d agreed", v-back[a], Display(a), maxGive[a])
		}
	}
	for _, g := range gets {
		found := false
		for _, o := range u.Tx.Outputs {
			if v, ok := explicitValue(o.Value); ok && bytes.Equal(o.Script, g.Script) && bytes.Equal(o.Asset[1:], g.Asset[:]) && v >= g.Value {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("the trade does not pay %d of %s to %x", g.Value, Display(g.Asset), g.Script)
		}
	}
	return nil
}

func outpointKey(txid string, vout uint32) string { return txid + ":" + strconv.Itoa(int(vout)) }

func explicitValue(v []byte) (uint64, bool) {
	if len(v) != 9 || v[0] != 1 {
		return 0, false
	}
	return binary.BigEndian.Uint64(v[1:]), true
}

// ExpirySig is the seller's BIP340 signature over
// tagged_hash("openamp-dvp-v1", txid || expires_at), txid the 32 bytes of the
// display id and expires_at 8 bytes big-endian: the server refuses the trade
// from expires_at on.
func ExpirySig(p *Party, txid string, expiresAt int64) (string, error) {
	b, err := hex.DecodeString(txid)
	if err != nil || len(b) != 32 {
		return "", fmt.Errorf("txid %q", txid)
	}
	var t [8]byte
	binary.BigEndian.PutUint64(t[:], uint64(expiresAt))
	msg := chainhash.TaggedHash([]byte("openamp-dvp-v1"), b, t[:])
	sig, err := schnorr.Sign(p.Key, msg[:])
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(sig.Serialize()), nil
}

// SellerSigs signs the seller's enclave inputs (the transfer leaf, SIGHASH_DEFAULT)
// and the trade's expiry.
func (p *Party) SellerSigs(u *Unsigned, ins []int, expiresAt int64) (map[string]string, string, error) {
	_, policyX, issuerX, err := p.Enclave()
	if err != nil {
		return nil, "", err
	}
	genesis, err := p.gen()
	if err != nil {
		return nil, "", err
	}
	enc, err := covenant.EnclaveTree(XOnly(p.Key), policyX, issuerX)
	if err != nil {
		return nil, "", err
	}
	tl := enc.Leaves["transfer"]
	sigs := map[string]string{}
	for _, i := range ins {
		s, err := Sign(p.Key, u.Sighash(i, tl.Script, genesis))
		if err != nil {
			return nil, "", err
		}
		sigs[strconv.Itoa(i)] = hex.EncodeToString(s)
	}
	es, err := ExpirySig(p, u.Txid(), expiresAt)
	return sigs, es, err
}

// BroadcastDvP asks A's policy server to complete the seller's inputs and
// broadcast the trade; every other input must already be signed.
func (p *Policy) BroadcastDvP(u *Unsigned, asset, sellerAID string, ins []int, userSigs map[string]string, expiresAt int64, expirySig string) (string, error) {
	raw, err := u.Hex()
	if err != nil {
		return "", err
	}
	r, err := p.Cosign(CosignRequest{Tx: raw, Asset: asset, SenderAID: sellerAID, Inputs: ins, Broadcast: true,
		UserSigs: userSigs, ExpiresAt: expiresAt, ExpirySig: expirySig})
	if err != nil {
		return "", err
	}
	if r.Txid == "" {
		return "", fmt.Errorf("the policy server broadcast nothing")
	}
	return r.Txid, nil
}

// CheckDvPWithServer runs every rule on the trade, signing nothing.
func (p *Policy) CheckDvPWithServer(u *Unsigned, asset, sellerAID string, ins []int) error {
	raw, err := u.Hex()
	if err != nil {
		return err
	}
	return p.Check(CosignRequest{Tx: raw, Asset: asset, SenderAID: sellerAID, Inputs: ins})
}
