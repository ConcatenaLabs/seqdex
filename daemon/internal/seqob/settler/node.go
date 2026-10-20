package settler

import (
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/matcher"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/watcher"
	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
	"github.com/vulpemventures/go-elements/elementsutil"
	"github.com/vulpemventures/go-elements/transaction"
)

// dustFold is the atom threshold below which a leftover bitcoin change is folded
// into the fee rather than emitted as a (potentially dust) change output.
const dustFold = 1000

// NodeFunder is the production FeeFunder: it funds the settler's bitcoin fee/gap
// from a node wallet (listunspent) and signs the bitcoin input(s) via
// signrawtransactionwithwallet. It touches ONLY the settler's own bitcoin — never
// a covenant input — so it cannot alter a maker payout; the worst a bug here can
// do is build a tx the covenants reject on broadcast.
type NodeFunder struct {
	chain     *xchain.Chain
	bitcoin   string // fee/gap asset DISPLAY hex (the listunspent "asset" field); the pegged asset by default, any accepted asset by -fee-asset
	btcCommit []byte // 0x01 || internal-order bitcoin asset (change/fee outputs)
	feeAtoms  uint64
}

// NewNodeFunder binds a funder to a wallet-scoped Chain. peggedDisplayHex is the
// chain's pegged "bitcoin" asset id (display hex, from getsidechaininfo);
// feeAtoms is the explicit network fee the settler pays per joint tx.
func NewNodeFunder(chain *xchain.Chain, peggedDisplayHex string, feeAtoms uint64) (*NodeFunder, error) {
	commit, err := elementsutil.AssetHashToBytes(peggedDisplayHex)
	if err != nil {
		return nil, fmt.Errorf("pegged asset hex %q: %w", peggedDisplayHex, err)
	}
	return &NodeFunder{
		chain:     chain,
		bitcoin:   strings.ToLower(peggedDisplayHex),
		btcCommit: commit,
		feeAtoms:  feeAtoms,
	}, nil
}

// GapSPK returns a fresh settler-owned (unblinded) bitcoin scriptPubKey.
func (f *NodeFunder) GapSPK() ([]byte, error) { return f.freshUnconfSPK() }

// FundAndSign appends the settler's bitcoin fee input(s), change, and the
// explicit fee output, then signs. It preserves output indices 0-3.
func (f *NodeFunder) FundAndSign(skeletonHex string, gapTotal uint64) (string, error) {
	tx, err := transaction.NewTxFromHex(skeletonHex)
	if err != nil {
		return "", fmt.Errorf("parse skeleton: %w", err)
	}

	target := gapTotal + f.feeAtoms
	utxos, sum, err := f.selectBitcoin(target)
	if err != nil {
		return "", err
	}
	for _, u := range utxos {
		h, herr := elementsutil.TxIDToBytes(u.txid)
		if herr != nil {
			return "", fmt.Errorf("utxo txid %q: %w", u.txid, herr)
		}
		tx.AddInput(transaction.NewTxInput(h, u.vout))
	}

	// Bitcoin balance for the settler side: input == gap outputs + change + fee.
	change := sum - gapTotal - f.feeAtoms
	fee := f.feeAtoms
	if change > 0 && change < dustFold {
		// Fold a dust change into the fee (still the settler's own money) so we
		// never emit a dust output the node would reject.
		fee += change
		change = 0
	}
	if change > 0 {
		changeSPK, serr := f.freshUnconfSPK()
		if serr != nil {
			return "", fmt.Errorf("change spk: %w", serr)
		}
		cv, verr := elementsutil.ValueToBytes(change)
		if verr != nil {
			return "", verr
		}
		tx.AddOutput(transaction.NewTxOutput(f.btcCommit, cv, changeSPK))
	}
	fv, err := elementsutil.ValueToBytes(fee)
	if err != nil {
		return "", err
	}
	tx.AddOutput(transaction.NewTxOutput(f.btcCommit, fv, []byte{})) // explicit fee

	rawHex, err := tx.ToHex()
	if err != nil {
		return "", err
	}

	// Sign the settler's bitcoin input(s). The covenant inputs are not the
	// wallet's, so signrawtransactionwithwallet leaves them unsigned (complete may
	// be false) — that is expected; the settler injects their witnesses next.
	var res struct {
		Hex      string `json:"hex"`
		Complete bool   `json:"complete"`
	}
	if err := f.chain.RPC().Call(&res, "signrawtransactionwithwallet", rawHex); err != nil {
		return "", fmt.Errorf("signrawtransactionwithwallet: %w", err)
	}
	if res.Hex == "" {
		return "", fmt.Errorf("signrawtransactionwithwallet returned empty hex")
	}
	return res.Hex, nil
}

type btcUTXO struct {
	txid  string
	vout  uint32
	atoms uint64
}

// selectBitcoin picks spendable bitcoin UTXOs from the fee wallet covering
// target atoms: the smallest single UTXO that covers it, else smallest-first
// accumulation. Returns the picked UTXOs and their summed atoms.
func (f *NodeFunder) selectBitcoin(target uint64) ([]btcUTXO, uint64, error) {
	var raw []struct {
		Txid          string  `json:"txid"`
		Vout          uint32  `json:"vout"`
		Amount        float64 `json:"amount"`
		Asset         string  `json:"asset"`
		Spendable     bool    `json:"spendable"`
		Confirmations int     `json:"confirmations"`
	}
	if err := f.chain.RPC().Call(&raw, "listunspent", 1); err != nil {
		return nil, 0, fmt.Errorf("listunspent: %w", err)
	}
	var cands []btcUTXO
	for _, u := range raw {
		if !u.Spendable {
			continue
		}
		a := strings.ToLower(u.Asset)
		if a != f.bitcoin && a != "bitcoin" {
			continue
		}
		cands = append(cands, btcUTXO{txid: u.Txid, vout: u.Vout, atoms: coinsToAtoms(u.Amount)})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].atoms < cands[j].atoms })

	// Prefer the smallest single UTXO that covers the target (minimal change).
	for _, u := range cands {
		if u.atoms >= target {
			return []btcUTXO{u}, u.atoms, nil
		}
	}
	// Otherwise accumulate smallest-first.
	var picked []btcUTXO
	var sum uint64
	for _, u := range cands {
		picked = append(picked, u)
		sum += u.atoms
		if sum >= target {
			return picked, sum, nil
		}
	}
	return nil, sum, fmt.Errorf("fee wallet has only %d bitcoin atoms across %d spendable utxo(s), need %d (gap+fee)", sum, len(cands), target)
}

// freshUnconfSPK returns a fresh UNBLINDED wallet scriptPubKey, mirroring the
// regtest wallet_spk(): getnewaddress, then resolve its unconfidential form and
// read that address' scriptPubKey. Sequentia defaults to transparent addresses,
// but forcing the unconfidential form keeps the settler's own outputs explicit
// even on a node configured with blinded addresses.
func (f *NodeFunder) freshUnconfSPK() ([]byte, error) {
	var addr string
	if err := f.chain.RPC().Call(&addr, "getnewaddress"); err != nil {
		return nil, fmt.Errorf("getnewaddress: %w", err)
	}
	spkHex, err := f.addrSPK(addr)
	if err != nil {
		return nil, err
	}
	return hex.DecodeString(spkHex)
}

// addrSPK returns the unconfidential scriptPubKey hex for an address.
func (f *NodeFunder) addrSPK(addr string) (string, error) {
	var info struct {
		Unconfidential string `json:"unconfidential"`
		ScriptPubKey   string `json:"scriptPubKey"`
	}
	if err := f.chain.RPC().Call(&info, "getaddressinfo", addr); err != nil {
		return "", fmt.Errorf("getaddressinfo: %w", err)
	}
	if info.Unconfidential != "" && info.Unconfidential != addr {
		var uinfo struct {
			ScriptPubKey string `json:"scriptPubKey"`
		}
		if err := f.chain.RPC().Call(&uinfo, "getaddressinfo", info.Unconfidential); err == nil && uinfo.ScriptPubKey != "" {
			return uinfo.ScriptPubKey, nil
		}
	}
	if info.ScriptPubKey == "" {
		return "", fmt.Errorf("no scriptPubKey for address %s", addr)
	}
	return info.ScriptPubKey, nil
}

// ChainBroadcaster implements Broadcaster over a node's sendrawtransaction.
type ChainBroadcaster struct{ Chain *xchain.Chain }

// Broadcast gates the fully-assembled joint-fill tx on testmempoolaccept, then
// submits it and returns its txid. A spend the node would reject (a covenant
// floor missed by one atom, a fee under the relay floor) used to be sent blind,
// fail, and be rebuilt with fresh addresses every pass.
func (b ChainBroadcaster) Broadcast(rawTxHex string) (string, error) {
	var accept []struct {
		Allowed      bool   `json:"allowed"`
		RejectReason string `json:"reject-reason"`
	}
	if err := b.Chain.RPC().Call(&accept, "testmempoolaccept", []string{rawTxHex}); err != nil {
		return "", fmt.Errorf("testmempoolaccept: %w", err)
	}
	if len(accept) == 0 || !accept[0].Allowed {
		reason := "unknown"
		if len(accept) > 0 && accept[0].RejectReason != "" {
			reason = accept[0].RejectReason
		}
		return "", fmt.Errorf("node would reject the joint fill (%s); not broadcast", reason)
	}
	return b.Chain.Broadcast(rawTxHex)
}

// Inspector reads a covenant outpoint's on-chain state at the current tip. It is
// the subset of watcher.ChainView the settler needs; watcher.NewRPCChain(...)
// satisfies it, so the same keyless read-only node client the relay's watcher
// uses can back the settler's covenant reads.
type Inspector interface {
	Inspect(txid string, vout uint32, orderSPKHex, assetADisplay string) (watcher.CovState, error)
}

// Resolve inspects both covenant UTXOs of a both-covenant match at the current
// tip, verifies each is an UNSPENT covenant whose on-chain scriptPubKey matches
// its advertised terms (Order.VerifyAgainstSPK, which also rejects a non-NUMS
// internal key — a maker-cancellable rug), and returns the atoms locked in each.
// It fails closed if either covenant is spent, a ghost, holds the wrong asset, or
// does not verify — dropping a stale or hostile advertised cross before any
// assembly.
func Resolve(m matcher.Match, view Inspector) (lockedA, lockedB uint64, err error) {
	if !m.BothCovenant() {
		return 0, 0, fmt.Errorf("not a both-covenant match")
	}
	// Past a covenant's REFUND expiry its maker can reclaim at any time and race
	// the joint fill; nothing is lost (first spend wins) but the attempt is wasted.
	// The lock is a height, or a median time judged by the tip's median time.
	if tv, ok := view.(interface {
		Tip() (string, int64, error)
	}); ok {
		if _, tip, terr := tv.Tip(); terr == nil {
			var mtp int64
			if mv, ok := view.(interface{ TipMedianTime() (int64, error) }); ok {
				mtp, _ = mv.TipMedianTime()
			}
			for _, ct := range []*seqobv1.CovenantTerms{m.RestingCovenant, m.IncomingCovenant} {
				exp := int64(ct.GetExpiryLocktime())
				if exp == 0 {
					continue
				}
				if open, _ := (covenant.Order{ExpiryLocktime: exp}).RefundOpen(tip, mtp); open {
					return 0, 0, fmt.Errorf("covenant %s:%d is past its refund expiry %d (tip %d, median time %d)", ct.GetCovenantTxid(), ct.GetCovenantVout(), exp, tip, mtp)
				}
			}
		}
	}
	lockedA, err = resolveOne(m.RestingCovenant, view)
	if err != nil {
		return 0, 0, fmt.Errorf("resting covenant: %w", err)
	}
	lockedB, err = resolveOne(m.IncomingCovenant, view)
	if err != nil {
		return 0, 0, fmt.Errorf("incoming covenant: %w", err)
	}
	return lockedA, lockedB, nil
}

func resolveOne(t *seqobv1.CovenantTerms, view Inspector) (uint64, error) {
	o, u, err := orderFromTerms(t)
	if err != nil {
		return 0, err
	}
	tap, err := o.Derive()
	if err != nil {
		return 0, fmt.Errorf("derive covenant: %w", err)
	}
	spkHex := hex.EncodeToString(tap.ScriptPubKey)
	assetADisplay := hex.EncodeToString(elementsutil.ReverseBytes(o.AssetA[:]))

	st, err := view.Inspect(u.Txid, u.Vout, spkHex, assetADisplay)
	if err != nil {
		return 0, fmt.Errorf("inspect %s:%d: %w", u.Txid, u.Vout, err)
	}
	if !st.Unspent {
		return 0, fmt.Errorf("covenant %s:%d is not an unspent utxo (spent or ghost)", u.Txid, u.Vout)
	}
	onchainSPK, err := hex.DecodeString(st.SPKHex)
	if err != nil {
		return 0, fmt.Errorf("bad on-chain spk hex: %w", err)
	}
	if _, err := o.VerifyAgainstSPK(onchainSPK, false); err != nil {
		return 0, fmt.Errorf("covenant %s:%d fails trustless verify: %w", u.Txid, u.Vout, err)
	}
	if !strings.EqualFold(st.AssetDisplay, assetADisplay) {
		return 0, fmt.Errorf("covenant %s:%d holds asset %s, want sold-asset %s", u.Txid, u.Vout, st.AssetDisplay, assetADisplay)
	}
	return st.Value, nil
}

// coinsToAtoms converts decimal coins to integer atoms (1e8 per coin).
func coinsToAtoms(v float64) uint64 {
	if v <= 0 {
		return 0
	}
	return uint64(math.Round(v * 1e8))
}
