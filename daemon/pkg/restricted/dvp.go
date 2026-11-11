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
	u, err := Build(coins, outs, 0)
	if err != nil {
		return nil, err
	}
	for _, in := range u.Tx.Inputs {
		in.Sequence = Sequence
	}
	return u, nil
}

// Sequence is every input's sequence in a trade: replaceable (BIP125), and
// with BIP68's disable flag set, so no relative lock is met.
const Sequence uint32 = 0xfffffffd

// TradeSide is what one side of a trade agreed to and holds: the whole of
// what it checks of the transaction before it signs anything of it.
type TradeSide struct {
	// Offered are the coins it puts in, wallet or enclave.
	Offered []Coin
	// WalletOwns are every outpoint its wallet would sign (a wallet signs
	// every input it owns).
	WalletOwns map[string]bool
	// Ours are the scripts it controls: its enclave and the scripts it named
	// for this trade. What the trade pays to them comes back to it.
	Ours [][]byte
	// Gives is the most it gives of each asset, its share of the fee
	// included.
	Gives map[[32]byte]uint64
	// Gets is what it is paid, each by an output of its own.
	Gets []Out
	// FeeFloor, when set, is the least fee it accepts in a fee asset; an
	// error refuses the asset.
	FeeFloor func(asset [32]byte) (uint64, error)
}

// CheckTrade is what each side checks of a trade before it signs, from the
// transaction itself:
//
//   - the lock time is 0 and every input's sequence disables BIP68, so no
//     path gated by a time sits in the trade: a gated order's or an HTLC's
//     keyless refund pays an enclave at its output 0, and that output would
//     otherwise count as ours twice (as the refund and as the trade);
//   - every input that is ours, by the wallet or by a script of ours, is one
//     we offered, and every coin we offered is spent;
//   - every output is explicit; each of Gets is paid by its own output, at a
//     script of ours;
//   - per asset, what comes back to us plus what we agreed to give covers
//     what we put in plus what we are owed: an output counts once;
//   - one asset pays the fee, at least FeeFloor of it.
func CheckTrade(u *Unsigned, s TradeSide) error {
	if u.Tx.Locktime != 0 {
		return fmt.Errorf("the trade has lock time %d, not 0", u.Tx.Locktime)
	}
	if len(u.Coins) != len(u.Tx.Inputs) {
		return fmt.Errorf("the trade names %d coins for %d inputs", len(u.Coins), len(u.Tx.Inputs))
	}
	isOurs := func(spk []byte) bool {
		for _, o := range s.Ours {
			if len(o) > 0 && bytes.Equal(o, spk) {
				return true
			}
		}
		return false
	}
	offered := map[string]bool{}
	in := map[[32]byte]uint64{}
	for _, c := range s.Offered {
		offered[outpointKey(c.Txid, c.Vout)] = true
		in[c.Asset] += c.Value
	}
	for i, ti := range u.Tx.Inputs {
		if ti.Sequence&(1<<31) == 0 {
			return fmt.Errorf("input %d has sequence %#x, which can meet a relative lock", i, ti.Sequence)
		}
		h, err := chainhash.NewHash(ti.Hash)
		if err != nil {
			return err
		}
		k := outpointKey(h.String(), ti.Index)
		if u.Coins[i].Txid != h.String() || u.Coins[i].Vout != ti.Index {
			return fmt.Errorf("input %d spends %s, and its coin is named %s:%d", i, k, u.Coins[i].Txid, u.Coins[i].Vout)
		}
		if offered[k] {
			delete(offered, k)
			continue
		}
		if s.WalletOwns[k] {
			return fmt.Errorf("the trade spends %s, a coin of ours not offered", k)
		}
		if isOurs(u.Coins[i].Script) {
			return fmt.Errorf("the trade spends %s at a script of ours, not offered", k)
		}
	}
	if len(offered) != 0 {
		return fmt.Errorf("the trade leaves out coins we offered")
	}
	back := map[[32]byte]uint64{}
	fee := map[[32]byte]uint64{}
	used := make([]bool, len(u.Tx.Outputs))
	values := make([]uint64, len(u.Tx.Outputs))
	assets := make([][32]byte, len(u.Tx.Outputs))
	for i, o := range u.Tx.Outputs {
		v, ok := explicitValue(o.Value)
		if !ok || len(o.Asset) != 33 || o.Asset[0] != 1 {
			return fmt.Errorf("the trade carries a confidential output")
		}
		copy(assets[i][:], o.Asset[1:])
		values[i] = v
		switch {
		case len(o.Script) == 0:
			fee[assets[i]] += v
		case isOurs(o.Script):
			back[assets[i]] += v
		}
	}
	owed := map[[32]byte]uint64{}
	for _, g := range s.Gets {
		if !isOurs(g.Script) {
			return fmt.Errorf("we are to be paid at %x, a script not ours", g.Script)
		}
		found := false
		for i, o := range u.Tx.Outputs {
			if !used[i] && bytes.Equal(o.Script, g.Script) && assets[i] == g.Asset && values[i] >= g.Value {
				used[i], found = true, true
				break
			}
		}
		if !found {
			return fmt.Errorf("the trade does not pay %d of %s to %x", g.Value, Display(g.Asset), g.Script)
		}
		owed[g.Asset] += g.Value
	}
	seen := map[[32]byte]bool{}
	for a := range in {
		seen[a] = true
	}
	for a := range owed {
		seen[a] = true
	}
	for a := range seen {
		if back[a]+s.Gives[a] < in[a]+owed[a] {
			if in[a] > back[a] {
				return fmt.Errorf("the trade takes %d of %s from us, more than the %d agreed", in[a]+owed[a]-back[a], Display(a), s.Gives[a])
			}
			return fmt.Errorf("the trade pays us %d of %s, less than the %d owed", back[a]-in[a], Display(a), owed[a])
		}
	}
	if len(fee) != 1 {
		return fmt.Errorf("the trade pays its fee in %d assets, not one", len(fee))
	}
	if s.FeeFloor != nil {
		for a, v := range fee {
			min, err := s.FeeFloor(a)
			if err != nil {
				return fmt.Errorf("the fee in %s: %v", Display(a), err)
			}
			if v < min {
				return fmt.Errorf("the trade pays a fee of %d %s, below our floor of %d", v, Display(a), min)
			}
		}
	}
	return nil
}

// OfferedInputs are the indices of the inputs spending the coins offered.
func OfferedInputs(u *Unsigned, offered []Coin) []int {
	set := map[string]bool{}
	for _, c := range offered {
		set[outpointKey(c.Txid, c.Vout)] = true
	}
	var ins []int
	for i, c := range u.Coins {
		if set[outpointKey(c.Txid, c.Vout)] {
			ins = append(ins, i)
		}
	}
	return ins
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
