package restricted

// A gated order: a holder's resting offer to sell a restricted asset while
// offline (openamp README, "Gated orders"). The maker moves the asset from its
// enclave into the order in one transaction its policy server co-signs as the
// maker's own move; buyers fill it through that server; after the expiry
// anyone sends what is left home to the maker's enclave, with no key.

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
)

// The policy server admits an order whose expiry is between 1 hour and 30 days
// ahead; the maker keeps a margin on both sides of that window.
const (
	MinGatedAhead = time.Hour + 10*time.Minute
	MaxGatedAhead = 30*24*time.Hour - 10*time.Minute
)

// GatedTerms are what a maker chooses for a gated order.
type GatedTerms struct {
	AssetB           string // display hex: the asset paid for it
	Value            uint64 // the restricted asset's atoms placed
	RateNum, RateDen uint64 // required_B = ceil(filled * RateNum / RateDen)
	MinLot           uint64
	Expiry           uint32 // Unix time: fills end and the refund opens
	FeeAsset         string // display hex: pays the placement's fee and the refund's
}

// Placed is a funded gated order, as the maker persists it.
type Placed struct {
	Terms     GatedTerms
	Order     covenant.GatedOrder
	Txid      string
	Vout      uint32
	MakerProg []byte // the v1 program the fills pay
	// Reserve is the wallet coin set aside to pay the refund's fee.
	Reserve Coin
}

// GatedOrder builds the order's terms from the policy server's records: the
// policy key, the clawback key and this maker's enclave.
func (p *Party) GatedOrder(t GatedTerms, makerProg []byte, expiry uint32) (covenant.GatedOrder, error) {
	var g covenant.GatedOrder
	enclaveSPK, policyX, issuerX, err := p.Enclave()
	if err != nil {
		return g, err
	}
	enc, err := ProgramOf(enclaveSPK)
	if err != nil {
		return g, err
	}
	a, err := ReverseHex(p.Asset)
	if err != nil {
		return g, err
	}
	b, err := ReverseHex(t.AssetB)
	if err != nil {
		return g, err
	}
	return covenant.GatedOrder{AssetA: a, AssetB: b, RateNum: t.RateNum, RateDen: t.RateDen, MinLot: t.MinLot,
		MakerProg: makerProg, PolicyX: policyX, IssuerX: issuerX, Expiry: expiry, MakerEnclave: enc}, nil
}

// PlaceGated moves t.Value of the asset from the maker's enclave into a gated
// order: output 0 the order, then the asset's change home to the enclave, the
// reserve coin for the refund's fee, the fee asset's change, and the fee.
func (p *Party) PlaceGated(t GatedTerms, rfaPerVb uint64) (*Placed, error) {
	now := p.Node.Now()
	if e := int64(t.Expiry); e < now.Add(MinGatedAhead).Unix() || e > now.Add(MaxGatedAhead).Unix() {
		return nil, fmt.Errorf("the expiry %d must be between %s and %s ahead", t.Expiry, MinGatedAhead, MaxGatedAhead)
	}
	if t.FeeAsset == p.Asset || t.AssetB == p.Asset {
		return nil, fmt.Errorf("neither the fee nor the price may be in the restricted asset itself")
	}
	payout, err := p.Node.NewV1Script()
	if err != nil {
		return nil, err
	}
	makerProg, err := ProgramOf(payout)
	if err != nil {
		return nil, err
	}
	g, err := p.GatedOrder(t, makerProg[:], t.Expiry)
	if err != nil {
		return nil, err
	}
	if err := (covenant.Order{RateNum: g.RateNum, RateDen: g.RateDen, MinLot: g.MinLot}).CheckTerms(); err != nil {
		return nil, err
	}
	if err := (covenant.Order{RateNum: g.RateNum, RateDen: g.RateDen}).CheckArithmetic(t.Value); err != nil {
		return nil, err
	}
	tree, err := g.Tree()
	if err != nil {
		return nil, err
	}
	fill, err := covenant.BuildFillLeaf(g.AssetA, g.AssetB, g.RateNum, g.RateDen, g.MinLot, g.MakerProg, 1)
	if err != nil {
		return nil, err
	}
	enclaveSPK, policyX, issuerX, err := p.Enclave()
	if err != nil {
		return nil, err
	}
	genesis, err := p.gen()
	if err != nil {
		return nil, err
	}
	refundFee, err := p.Node.FeeAtoms(t.FeeAsset, 400, rfaPerVb)
	if err != nil {
		return nil, err
	}
	placeFee, err := p.Node.FeeAtoms(t.FeeAsset, 900, rfaPerVb)
	if err != nil {
		return nil, err
	}
	coins, err := p.Node.EnclaveCoins(enclaveSPK, p.Asset)
	if err != nil {
		return nil, err
	}
	var pick []Coin
	var sum uint64
	for _, c := range coins {
		if sum >= t.Value {
			break
		}
		pick = append(pick, c)
		sum += c.Value
	}
	if sum < t.Value {
		return nil, fmt.Errorf("the enclave holds %d of the %d to place", sum, t.Value)
	}
	feeCoins, err := p.Node.WalletCoins(t.FeeAsset, placeFee+refundFee)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Placed, error) {
		_ = p.Node.Unlock(feeCoins)
		return nil, err
	}
	var haveF uint64
	for _, c := range feeCoins {
		haveF += c.Value
	}
	change, err := p.Node.NewScript()
	if err != nil {
		return fail(err)
	}
	reserveSPK, err := p.Node.NewScript()
	if err != nil {
		return fail(err)
	}
	asset, _ := ReverseHex(p.Asset)
	feeAsset, err := ReverseHex(t.FeeAsset)
	if err != nil {
		return fail(err)
	}
	outs := []Out{{asset, t.Value, tree.ScriptPubKey}}
	if sum > t.Value {
		outs = append(outs, Out{asset, sum - t.Value, enclaveSPK})
	}
	reserveVout := uint32(len(outs))
	outs = append(outs, Out{feeAsset, refundFee, reserveSPK})
	if ch := haveF - placeFee - refundFee; ch > 0 {
		outs = append(outs, Out{feeAsset, ch, change})
	}
	outs = append(outs, Out{feeAsset, placeFee, nil})
	u, err := Build(append(append([]Coin{}, pick...), feeCoins...), outs, 0)
	if err != nil {
		return fail(err)
	}
	txid := u.Txid()
	ssig, err := SenderSig(p.Key, txid)
	if err != nil {
		return fail(err)
	}
	ins := make([]int, len(pick))
	for i := range pick {
		ins[i] = i
	}
	raw, _ := u.Hex()
	resp, err := p.Policy.Cosign(CosignRequest{Tx: raw, Asset: p.Asset, SenderAID: p.AID, Inputs: ins, SenderSig: ssig,
		OrderOutputs: []OrderOutput{{Output: 0, Fill: hex.EncodeToString(fill), Expiry: t.Expiry}}})
	if err != nil {
		return fail(err)
	}
	enc, err := covenant.EnclaveTree(XOnly(p.Key), policyX, issuerX)
	if err != nil {
		return fail(err)
	}
	tl := enc.Leaves["transfer"]
	for _, i := range ins {
		psig, h, err := u.CheckedPolicySig(resp, i, tl.Script, tl.Control, policyX, genesis)
		if err != nil {
			return fail(err)
		}
		usig, err := Sign(p.Key, h)
		if err != nil {
			return fail(err)
		}
		u.Tx.Inputs[i].Witness = EnclaveWitness(psig, usig, tl.Script, tl.Control)
	}
	if err := p.Node.SignWallet(u); err != nil {
		return fail(err)
	}
	if _, err := p.Node.Broadcast(u); err != nil {
		return fail(err)
	}
	pl := &Placed{Terms: t, Order: g, Txid: txid, Vout: 0, MakerProg: makerProg[:],
		Reserve: Coin{Txid: txid, Vout: reserveVout, Asset: feeAsset, Value: refundFee, Script: reserveSPK}}
	_ = p.Node.Lock([]Coin{pl.Reserve})
	return pl, nil
}

// RefundGated sends what is left of a gated order home to the maker's enclave
// once its expiry has passed: the order at input 0, its whole value at output
// 0, the reserved coin as the fee. It returns "" when the order is gone
// (filled whole, or already sent home).
func (p *Party) RefundGated(pl *Placed) (string, error) {
	if now := p.Node.Now(); now.Unix() < int64(pl.Order.Expiry) {
		return "", fmt.Errorf("the refund opens at the expiry %d (now %d)", pl.Order.Expiry, now.Unix())
	}
	tree, err := pl.Order.Tree()
	if err != nil {
		return "", err
	}
	// A partial fill leaves the remainder at the same script at 2k+1 of the
	// fill: follow the order's coin down the chain of fills.
	txid, vout := pl.Txid, pl.Vout
	for i := 0; i < 1000; i++ {
		c, err := p.Node.CoinAt(txid, vout)
		if err != nil {
			return "", err
		}
		if c != nil {
			if !bytes.Equal(c.Script, tree.ScriptPubKey) {
				return "", fmt.Errorf("%s:%d is not the order", txid, vout)
			}
			home := append([]byte{0x51, 0x20}, pl.Order.MakerEnclave[:]...)
			u, err := BuildRefund(RefundPlan{Coin: *c, Home: home,
				Leaf: covenant.TapLeaf{Script: tree.RefundLeaf, Control: tree.RefundControl}, FeeCoin: pl.Reserve, TSeq: pl.Order.Expiry})
			if err != nil {
				return "", err
			}
			if err := p.Node.SignWallet(u); err != nil {
				return "", err
			}
			return p.Node.Broadcast(u)
		}
		spender, _, err := p.Node.SpendWitness(txid, vout)
		if err != nil {
			return "", err
		}
		if spender == "" {
			return "", fmt.Errorf("%s:%d is neither unspent nor spent on the chain", txid, vout)
		}
		next, ok, err := p.remainderOf(spender, tree.ScriptPubKey)
		if err != nil {
			return "", err
		}
		if !ok {
			_ = p.Node.Unlock([]Coin{pl.Reserve})
			return "", nil
		}
		txid, vout = spender, next
	}
	return "", fmt.Errorf("the order's chain of fills is longer than followed")
}

// remainderOf finds the output of tx at the order's script: a partial fill's
// remainder.
func (p *Party) remainderOf(txid string, spk []byte) (uint32, bool, error) {
	var raw struct {
		Vout []struct {
			N            uint32 `json:"n"`
			ScriptPubKey struct {
				Hex string `json:"hex"`
			} `json:"scriptPubKey"`
		} `json:"vout"`
	}
	if err := p.Node.rpc().Call(&raw, "getrawtransaction", txid, true); err != nil {
		return 0, false, err
	}
	want := hex.EncodeToString(spk)
	for _, o := range raw.Vout {
		if o.ScriptPubKey.Hex == want {
			return o.N, true, nil
		}
	}
	return 0, false, nil
}
