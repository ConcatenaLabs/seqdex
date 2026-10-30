package xchain

import (
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"
)

// KeepEvery is how often a keeper looks at the Bitcoin payments it keeps.
var KeepEvery = 60 * time.Second

// pendingSpend is an HTLC claim or refund the keeper holds until it confirms.
type pendingSpend struct {
	kind     string // "claim" or "refund"
	fundTxid string
	vout     uint32
	amount   uint64
	txid     string
	fee      uint64
	vsize    int
	rebuild  func(fee uint64) (string, error)
	cache    *spendCache
}

// btcKeeper keeps a backend's Bitcoin payments moving until they confirm:
//   - an HTLC claim or refund still in the mempool when the going rate has
//     risen past what it pays is replaced (RBF) by the same spend at the going
//     rate, its fee raised by at least what BIP125 asks and never past half the
//     output: a claim must confirm before the other side's timelock lets it
//     refund, and a refund before the other side, holding the preimage, could
//     still claim;
//   - an HTLC funding is lifted by CPFP (its txid, which the other side was
//     told, stays): the wallet spends the funding's own change to itself at the
//     going rate, and Bitcoin Core (v26+) prices the child so the pair pays it.
//
// It runs while it holds something and stops when nothing is left;
// SettleBTCSpends drives every keeper from a process that is about to exit.
type btcKeeper struct {
	chain    *BitcoinChain
	mu       sync.Mutex
	spends   map[string]*pendingSpend // fundTxid:vout:kind
	fundings map[string]string        // funding txid -> its newest CPFP child ("" none)
	running  bool
	noCPFP   map[string]bool // fundings with no change of ours to lift them by
}

var (
	keepersMu sync.Mutex
	keepers   []*btcKeeper
)

func newBTCKeeper(c *BitcoinChain) *btcKeeper {
	k := &btcKeeper{chain: c, spends: map[string]*pendingSpend{}, fundings: map[string]string{}, noCPFP: map[string]bool{}}
	keepersMu.Lock()
	keepers = append(keepers, k)
	keepersMu.Unlock()
	return k
}

func (k *btcKeeper) watchSpend(p *pendingSpend) {
	k.mu.Lock()
	k.spends[fmt.Sprintf("%s:%d:%s", p.fundTxid, p.vout, p.kind)] = p
	k.mu.Unlock()
	k.start()
}

func (k *btcKeeper) watchFunding(txid string) {
	k.mu.Lock()
	if _, ok := k.fundings[txid]; !ok {
		k.fundings[txid] = ""
	}
	k.mu.Unlock()
	k.start()
}

func (k *btcKeeper) start() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.running {
		return
	}
	k.running = true
	go func() {
		for {
			time.Sleep(KeepEvery)
			if k.pass() == 0 {
				k.mu.Lock()
				if len(k.spends)+len(k.fundings) == 0 {
					k.running = false
					k.mu.Unlock()
					return
				}
				k.mu.Unlock()
			}
		}
	}()
}

// pending is how many spends and fundings the keeper still holds.
func (k *btcKeeper) pending() (spends, fundings int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.spends), len(k.fundings)
}

// pass looks at each payment once: drops the confirmed, bumps the stalled.
// It returns how many are left.
func (k *btcKeeper) pass() int {
	going, why := k.chain.GoingRate()
	k.mu.Lock()
	spends := make([]*pendingSpend, 0, len(k.spends))
	keys := make([]string, 0, len(k.spends))
	for key, p := range k.spends {
		spends = append(spends, p)
		keys = append(keys, key)
	}
	fundings := make(map[string]string, len(k.fundings))
	for t, c := range k.fundings {
		fundings[t] = c
	}
	k.mu.Unlock()
	for i, p := range spends {
		if done := k.keepSpend(p, going, why); done {
			k.mu.Lock()
			delete(k.spends, keys[i])
			k.mu.Unlock()
		}
	}
	for t, child := range fundings {
		done, newChild := k.keepFunding(t, child, going, why)
		k.mu.Lock()
		if done {
			delete(k.fundings, t)
		} else if newChild != "" {
			k.fundings[t] = newChild
		}
		k.mu.Unlock()
	}
	s, f := k.pending()
	return s + f
}

type mempoolEntry struct {
	VSize          int64 `json:"vsize"`
	DescendantSize int64 `json:"descendantsize"`
	Fees           struct {
		Base       float64 `json:"base"`
		Descendant float64 `json:"descendant"`
	} `json:"fees"`
}

func (k *btcKeeper) inMempool(txid string) (*mempoolEntry, bool) {
	var e mempoolEntry
	if err := k.chain.RPC().Call(&e, "getmempoolentry", txid); err != nil {
		return nil, false
	}
	return &e, true
}

func (k *btcKeeper) confirmed(txid string) bool {
	var r struct {
		Confirmations int `json:"confirmations"`
	}
	if k.chain.RPC().Call(&r, "getrawtransaction", txid, true) == nil && r.Confirmations > 0 {
		return true
	}
	return k.chain.RPC().Call(&r, "gettransaction", txid) == nil && r.Confirmations > 0
}

// keepSpend: true once the spend is confirmed, or the output it spends is
// taken by another transaction.
func (k *btcKeeper) keepSpend(p *pendingSpend, going float64, why string) bool {
	if _, in := k.inMempool(p.txid); !in {
		if k.confirmed(p.txid) {
			return true
		}
		var out map[string]interface{}
		if err := k.chain.RPC().Call(&out, "gettxout", p.fundTxid, p.vout, true); err == nil && out == nil {
			log.Printf("xchain/btc: the HTLC %s:%d our %s %s spent is spent by another transaction", p.fundTxid, p.vout, p.kind, p.txid)
			return true
		}
		// Neither in the mempool nor in a block, the output still there: send it again.
		if raw, err := p.rebuild(p.fee); err == nil {
			if _, err := k.chain.Broadcast(raw); err != nil {
				log.Printf("xchain/btc: %s %s re-sent and refused: %v", p.kind, p.txid, err)
			}
		}
		return false
	}
	rate := float64(p.fee) / float64(p.vsize)
	if going <= rate+0.5 {
		return false
	}
	// BIP125: the replacement pays at least the old fee plus the incremental
	// relay rate (1 sat/vB) on its own size.
	fee := uint64(math.Ceil(going * float64(p.vsize)))
	if min := p.fee + uint64(p.vsize) + 1; fee < min {
		fee = min
	}
	if max := p.amount / 2; fee > max {
		fee = max
	}
	if fee <= p.fee {
		log.Printf("xchain/btc: %s %s pays %.1f sat/vB, the going rate is %.1f (%s), and half its output is the most it may pay", p.kind, p.txid, rate, going, why)
		return false
	}
	raw, err := p.rebuild(fee)
	if err != nil {
		log.Printf("xchain/btc: rebuild %s %s at %d sats: %v", p.kind, p.txid, fee, err)
		return false
	}
	txid, err := k.chain.Broadcast(raw)
	if err != nil {
		log.Printf("xchain/btc: bump %s %s to %d sats refused: %v", p.kind, p.txid, fee, err)
		return false
	}
	log.Printf("xchain/btc: %s %s bumped from %.1f to %.1f sat/vB (%s): now %s", p.kind, p.txid, rate, float64(fee)/float64(p.vsize), why, txid)
	p.cache.replace(p.fundTxid, p.vout, p.kind, raw)
	p.txid, p.fee = txid, fee
	return false
}

// keepFunding: done once the funding is confirmed; else, when it and our
// children of it pay under the going rate, a new child at the going rate.
func (k *btcKeeper) keepFunding(txid, child string, going float64, why string) (bool, string) {
	e, in := k.inMempool(txid)
	if !in {
		return k.confirmed(txid), ""
	}
	pkg := e.Fees.Descendant * 1e8 / float64(e.DescendantSize)
	own := e.Fees.Base * 1e8 / float64(e.VSize)
	if math.Max(pkg, own) >= going-0.5 || k.noCPFP[txid] {
		return false, ""
	}
	// Our unspent output of the newest transaction in the chain: the child's, or
	// the funding's change.
	var utxos []struct {
		TxID      string  `json:"txid"`
		Vout      uint32  `json:"vout"`
		Amount    float64 `json:"amount"`
		Spendable bool    `json:"spendable"`
	}
	if err := k.chain.RPC().Call(&utxos, "listunspent", 0, 0); err != nil {
		return false, ""
	}
	pick := -1
	for i, u := range utxos {
		if !u.Spendable {
			continue
		}
		if child != "" && u.TxID == child {
			pick = i
			break
		}
		if u.TxID == txid && pick < 0 {
			pick = i
		}
	}
	if pick < 0 {
		k.noCPFP[txid] = true
		log.Printf("xchain/btc: funding %s pays %.1f sat/vB under the going rate %.1f and holds no change of ours to lift it by (CPFP)", txid, math.Max(pkg, own), going)
		return false, ""
	}
	var to string
	if err := k.chain.RPC().Call(&to, "getrawchangeaddress"); err != nil {
		return false, ""
	}
	var res struct {
		TxID string `json:"txid"`
	}
	opts := map[string]interface{}{
		"inputs":   []map[string]interface{}{{"txid": utxos[pick].TxID, "vout": utxos[pick].Vout}},
		"fee_rate": going,
	}
	if err := k.chain.RPC().CallNamed(&res, "sendall", map[string]interface{}{"recipients": []string{to}, "options": opts}); err != nil {
		if !strings.Contains(err.Error(), "Insufficient") {
			log.Printf("xchain/btc: CPFP of funding %s: %v", txid, err)
		}
		return false, ""
	}
	log.Printf("xchain/btc: funding %s paid %.1f sat/vB with our children; lifted to %.1f (%s) by CPFP %s", txid, math.Max(pkg, own), going, why, res.TxID)
	return false, res.TxID
}

// SettleBTCSpends drives every keeper of this process until its HTLC claims
// and refunds are confirmed, or until timeout: what a command that broadcast
// one runs before it exits, since no keeper outlives its process. It returns
// an error naming what is still unconfirmed. Fundings are not waited for: a
// command that funds waits for its funding itself.
func SettleBTCSpends(timeout time.Duration, logf func(string, ...interface{})) error {
	keepersMu.Lock()
	ks := append([]*btcKeeper(nil), keepers...)
	keepersMu.Unlock()
	deadline := time.Now().Add(timeout)
	for {
		left := 0
		for _, k := range ks {
			if s, _ := k.pending(); s > 0 {
				k.pass()
			}
			s, _ := k.pending()
			left += s
		}
		if left == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d Bitcoin HTLC spend(s) still unconfirmed after %s", left, timeout)
		}
		if logf != nil {
			logf("waiting for %d Bitcoin HTLC spend(s) to confirm (bumped to the going rate while they wait)", left)
		}
		time.Sleep(KeepEvery)
	}
}
