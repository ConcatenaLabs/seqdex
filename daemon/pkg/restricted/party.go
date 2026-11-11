package restricted

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"

	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
)

// The clock of a restricted swap. Every lock is a Unix time on both chains.
const (
	// TSeqAhead is how far after its lock the HTLC's refund opens.
	TSeqAhead = 4 * time.Hour
	// MinTSeqAhead is the least a seller locks with: the policy server admits
	// an HTLC whose T_seq is at least 2 h ahead; ten minutes more cover the
	// time between this check and the server's.
	MinTSeqAhead = 2*time.Hour + 10*time.Minute
	// BtcAfterSeq is the least T_btc - T_seq: the BTC refund opens a day after
	// the asset's, so the seller always has a day to claim the BTC with s.
	BtcAfterSeq = 24 * time.Hour
	// TBtcAhead is T_btc's distance from the terms.
	TBtcAhead = 30 * time.Hour
	// ClaimCutoff is how long before T_seq the buyer stops claiming: the
	// policy server co-signs no claim after T_seq - 1 h, and a buyer never
	// reveals s after it.
	ClaimCutoff = time.Hour
	// BtcClaimGuard: no claim once Bitcoin's median time is within 12 h of
	// T_btc, whatever the Sequentia clock says.
	BtcClaimGuard = 12 * time.Hour
)

// PickTSeq is the seller's T_seq at its lock: 4 h ahead, earlier when T_btc
// requires it, and never less than MinTSeqAhead ahead.
func PickTSeq(now time.Time, tBtc uint32) (uint32, error) {
	t := now.Add(TSeqAhead).Unix()
	if lim := int64(tBtc) - int64(BtcAfterSeq/time.Second); t > lim {
		t = lim
	}
	if t < now.Add(MinTSeqAhead).Unix() {
		return 0, fmt.Errorf("too late to lock: T_btc %d leaves no T_seq %s ahead and %s before it", tBtc, MinTSeqAhead, BtcAfterSeq)
	}
	return uint32(t), nil
}

// CheckLocks is what both sides check of a swap's two locks.
func CheckLocks(tSeq, tBtc uint32) error {
	if tSeq < covenant.MinTimeLock || tBtc < covenant.MinTimeLock {
		return fmt.Errorf("a lock is a height (T_seq %d, T_btc %d): a restricted swap's locks are times", tSeq, tBtc)
	}
	if int64(tBtc)-int64(tSeq) < int64(BtcAfterSeq/time.Second) {
		return fmt.Errorf("T_btc %d is less than %s after T_seq %d", tBtc, BtcAfterSeq, tSeq)
	}
	return nil
}

// ErrNoClaim is a claim the buyer must not make: it keeps s, and both refund.
var ErrNoClaim = errors.New("no claim")

// ClaimAllowed says whether the buyer may still reveal s. now is the later of
// its clock and Sequentia's median time; btcMTP is Bitcoin's median time.
func ClaimAllowed(now time.Time, btcMTP int64, tSeq, tBtc uint32) error {
	if cut := int64(tSeq) - int64(ClaimCutoff/time.Second); now.Unix() >= cut {
		return fmt.Errorf("%w: the cut-off %s has passed; the HTLC goes home at T_seq", ErrNoClaim, time.Unix(cut, 0).UTC().Format(time.RFC3339))
	}
	if btcMTP >= int64(tBtc)-int64(BtcClaimGuard/time.Second) {
		return fmt.Errorf("%w: Bitcoin's median time %d is within %s of T_btc %d", ErrNoClaim, btcMTP, BtcClaimGuard, tBtc)
	}
	return nil
}

// Party is one side of a restricted trade: its account at the asset's policy
// server, its enclave key, and the node and wallet it pays fees from.
type Party struct {
	Node    *Node
	Policy  *Policy
	AID     string
	Key     *btcec.PrivateKey // the account's enclave key
	Asset   string            // display hex
	genesis *[32]byte
}

func (p *Party) gen() ([32]byte, error) {
	if p.genesis != nil {
		return *p.genesis, nil
	}
	g, err := p.Node.Genesis()
	if err != nil {
		return g, err
	}
	p.genesis = &g
	return g, nil
}

// Enclave returns this party's enclave script for the asset, checked to be the
// one its own key, the asset's policy key and its clawback key build.
func (p *Party) Enclave() ([]byte, [32]byte, *[32]byte, error) {
	a, err := p.Policy.Asset(p.Asset)
	if err != nil {
		return nil, [32]byte{}, nil, err
	}
	policyX, issuerX, err := a.Keys()
	if err != nil {
		return nil, policyX, nil, err
	}
	spk, err := p.Policy.EnclaveScript(p.AID, p.Asset)
	if err != nil {
		return nil, policyX, nil, err
	}
	tree, err := covenant.EnclaveTree(XOnly(p.Key), policyX, issuerX)
	if err != nil {
		return nil, policyX, nil, err
	}
	if !bytes.Equal(tree.ScriptPubKey, spk) {
		return nil, policyX, nil, fmt.Errorf("the policy server's enclave for %s is not the one this key builds", p.AID)
	}
	return spk, policyX, issuerX, nil
}

// SwapTerms are the terms the two sides agree, from which each rebuilds the
// HTLC and the pot.
type SwapTerms struct {
	Hash      [32]byte
	SellerAID string
	BuyerAID  string
	TSeq      uint32
	Value     uint64
	PotAsset  string // display hex
	PotValue  uint64
	Payout    []byte // the seller's v1 script the pot goes home to
}

// Rebuild derives the swap from the terms and the policy server's records.
func (p *Party) Rebuild(t SwapTerms) (Swap, error) {
	var s Swap
	a, err := p.Policy.Asset(p.Asset)
	if err != nil {
		return s, err
	}
	policyX, issuerX, err := a.Keys()
	if err != nil {
		return s, err
	}
	buyerX, err := p.Policy.UserKey(t.BuyerAID)
	if err != nil {
		return s, err
	}
	sellerSPK, err := p.Policy.EnclaveScript(t.SellerAID, p.Asset)
	if err != nil {
		return s, err
	}
	enc, _ := ProgramOf(sellerSPK)
	payout, err := ProgramOf(t.Payout)
	if err != nil {
		return s, fmt.Errorf("the pot's way home: %w", err)
	}
	asset, err := ReverseHex(p.Asset)
	if err != nil {
		return s, err
	}
	pot, err := ReverseHex(t.PotAsset)
	if err != nil {
		return s, err
	}
	return Swap{Hash: t.Hash, BuyerX: buyerX, PolicyX: policyX, IssuerX: issuerX, TSeq: t.TSeq,
		SellerEnclave: enc, Asset: asset, Value: t.Value, PotAsset: pot, PotValue: t.PotValue, SellerPayout: payout}, nil
}

// Locked is a funded swap, as the seller persists it.
type Locked struct {
	Terms    SwapTerms
	Txid     string
	HTLCVout uint32
	PotVout  uint32
	// Reserve holds the wallet coins the lock set aside to pay the two refunds'
	// fees, so a refund never waits on a coin the wallet spent meanwhile.
	Reserve []Coin
}

// Lock moves value of the asset from the seller's enclave into the HTLC, with
// the pot beside it, as one transaction its policy server co-signs. The lock
// also sets aside two coins of the pot's asset for the refunds' fees.
func (p *Party) Lock(t SwapTerms, rfaPerVb uint64) (*Locked, error) {
	if t.SellerAID != p.AID {
		return nil, fmt.Errorf("the lock is the seller's")
	}
	if t.Payout == nil {
		v1, err := p.Node.NewV1Script()
		if err != nil {
			return nil, err
		}
		t.Payout = v1
	}
	enclaveSPK, _, _, err := p.Enclave()
	if err != nil {
		return nil, err
	}
	s, err := p.Rebuild(t)
	if err != nil {
		return nil, err
	}
	genesis, err := p.gen()
	if err != nil {
		return nil, err
	}
	refundFee, err := p.Node.FeeAtoms(t.PotAsset, 400, rfaPerVb)
	if err != nil {
		return nil, err
	}
	reserve := 2 * refundFee
	lockFee, err := p.Node.FeeAtoms(t.PotAsset, 900, rfaPerVb)
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
	feeCoins, err := p.Node.WalletCoins(t.PotAsset, t.PotValue+lockFee+reserve)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Locked, error) {
		_ = p.Node.Unlock(feeCoins)
		return nil, err
	}
	change, err := p.Node.NewScript()
	if err != nil {
		return fail(err)
	}
	r1, err := p.Node.NewScript()
	if err != nil {
		return fail(err)
	}
	// Two reserve coins, each paying one refund's fee.
	rs := []Out{{s.PotAsset, refundFee, r1}, {s.PotAsset, refundFee, r1}}
	u, err := BuildLock(LockPlan{Swap: s, EnclaveCoins: pick, FeeCoins: feeCoins, FeeChange: change, Fee: lockFee, Reserve: rs})
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
		HTLCOutputs: []HTLCOutput{{Output: 0, BuyerAID: t.BuyerAID, Hash: hex.EncodeToString(t.Hash[:]), TSeq: t.TSeq}}})
	if err != nil {
		return fail(err)
	}
	a, _ := p.Policy.Asset(p.Asset)
	policyX, issuerX, _ := a.Keys()
	enc, _ := covenant.EnclaveTree(XOnly(p.Key), policyX, issuerX)
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
	l := &Locked{Terms: t, Txid: txid, HTLCVout: 0, PotVout: 1}
	for i, o := range u.Tx.Outputs {
		if bytes.Equal(o.Script, r1) {
			c := Coin{Txid: txid, Vout: uint32(i), Asset: s.PotAsset, Value: refundFee, Script: r1}
			l.Reserve = append(l.Reserve, c)
		}
	}
	_ = p.Node.Lock(l.Reserve)
	return l, nil
}

// Refund sends the HTLC and the pot home once T_seq has passed, each in its
// own transaction (a refund is input 0), each fee paid from a reserved coin.
// A coin already spent (claimed, or refunded by someone else) is skipped.
func (p *Party) Refund(l *Locked, rfaPerVb uint64) ([]string, error) {
	s, err := p.Rebuild(l.Terms)
	if err != nil {
		return nil, err
	}
	htlc, pot, err := s.Trees()
	if err != nil {
		return nil, err
	}
	if now := p.Node.Now(); now.Unix() < int64(s.TSeq) {
		return nil, fmt.Errorf("the refund opens at T_seq %d (now %d)", s.TSeq, now.Unix())
	}
	type leg struct {
		vout uint32
		leaf covenant.TapLeaf
		home []byte
	}
	legs := []leg{
		{l.HTLCVout, htlc.Leaves["refund"], append([]byte{0x51, 0x20}, s.SellerEnclave[:]...)},
		{l.PotVout, pot.Leaves["refund"], append([]byte{0x51, 0x20}, s.SellerPayout[:]...)},
	}
	var txids []string
	for i, lg := range legs {
		c, err := p.Node.CoinAt(l.Txid, lg.vout)
		if err != nil {
			return txids, err
		}
		if c == nil {
			continue
		}
		if i >= len(l.Reserve) {
			return txids, fmt.Errorf("no reserved fee coin for refund %d", i)
		}
		fc := l.Reserve[i]
		fee, err := p.Node.FeeAtoms(Display(fc.Asset), 400, rfaPerVb)
		if err != nil {
			return txids, err
		}
		if fee >= fc.Value {
			fee = fc.Value - 1
		}
		ch, err := p.Node.NewScript()
		if err != nil {
			return txids, err
		}
		u, err := BuildRefund(RefundPlan{Coin: *c, Home: lg.home, Leaf: lg.leaf, FeeCoin: fc, FeeChange: ch, Fee: fee, TSeq: s.TSeq})
		if err != nil {
			return txids, err
		}
		if err := p.Node.SignWallet(u); err != nil {
			return txids, err
		}
		txid, err := p.Node.Broadcast(u)
		if err != nil {
			return txids, err
		}
		txids = append(txids, txid)
	}
	return txids, nil
}

// WatchSecret reads s from the pot's claim once the buyer has made it. It
// returns "" while the pot is unspent, and an error if the pot went home
// (refunded) instead.
func (p *Party) WatchSecret(l *Locked) (secret []byte, claimTxid string, err error) {
	txid, w, err := p.Node.SpendWitness(l.Txid, l.PotVout)
	if err != nil || txid == "" {
		return nil, "", err
	}
	s, ok := covenant.PotSecret(w, l.Terms.Hash)
	if !ok {
		return nil, txid, fmt.Errorf("the pot was spent by %s without the secret: it went home", txid)
	}
	return s, txid, nil
}

// Claim takes the HTLC and the pot with s: the policy server co-signs the
// HTLC's input, judged by every rule with the seller as sender; the buyer
// signs both and adds s. The pot pays the fee.
func (p *Party) Claim(t SwapTerms, htlcAt, potAt Outpoint, secret []byte, rfaPerVb uint64) (string, error) {
	if t.BuyerAID != p.AID {
		return "", fmt.Errorf("the claim is the buyer's")
	}
	s, err := p.Rebuild(t)
	if err != nil {
		return "", err
	}
	htlc, pot, err := s.Trees()
	if err != nil {
		return "", err
	}
	hc, err := p.Node.CoinAt(htlcAt.Txid, htlcAt.Vout)
	if err != nil || hc == nil {
		return "", fmt.Errorf("the HTLC %s:%d is not unspent: %v", htlcAt.Txid, htlcAt.Vout, err)
	}
	pc, err := p.Node.CoinAt(potAt.Txid, potAt.Vout)
	if err != nil || pc == nil {
		return "", fmt.Errorf("the pot %s:%d is not unspent: %v", potAt.Txid, potAt.Vout, err)
	}
	if !bytes.Equal(hc.Script, htlc.ScriptPubKey) || hc.Asset != s.Asset || hc.Value != s.Value {
		return "", fmt.Errorf("%s:%d is not the HTLC the terms build", htlcAt.Txid, htlcAt.Vout)
	}
	if !bytes.Equal(pc.Script, pot.ScriptPubKey) || pc.Asset != s.PotAsset || pc.Value != s.PotValue {
		return "", fmt.Errorf("%s:%d is not the pot the terms build", potAt.Txid, potAt.Vout)
	}
	enclaveSPK, _, _, err := p.Enclave()
	if err != nil {
		return "", err
	}
	change, err := p.Node.NewScript()
	if err != nil {
		return "", err
	}
	fee, err := p.Node.FeeAtoms(t.PotAsset, 500, rfaPerVb)
	if err != nil {
		return "", err
	}
	u, err := BuildClaim(ClaimPlan{Swap: s, HTLC: *hc, Pot: *pc, BuyerEnclave: enclaveSPK, PotChange: change, Fee: fee})
	if err != nil {
		return "", err
	}
	genesis, err := p.gen()
	if err != nil {
		return "", err
	}
	raw, _ := u.Hex()
	resp, err := p.Policy.Cosign(CosignRequest{Tx: raw, Asset: p.Asset, SenderAID: t.SellerAID, HTLCInputs: []HTLCInput{{Input: 0}}})
	if err != nil {
		return "", err
	}
	cl := htlc.Leaves["claim"]
	psig, _, err := u.CheckedPolicySig(resp, 0, cl.Script, cl.Control, s.PolicyX, genesis)
	if err != nil {
		return "", err
	}
	if err := FinishClaim(u, s, secret, psig, p.Key, genesis); err != nil {
		return "", err
	}
	return p.Node.Broadcast(u)
}

// Outpoint names a coin.
type Outpoint struct {
	Txid string
	Vout uint32
}
