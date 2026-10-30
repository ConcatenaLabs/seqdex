package xchain

import (
	"crypto/rand"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
)

// The keeper against a live bitcoind regtest node with a funded wallet, named
// by SEQDEX_BTC_REGTEST=host:port:user:password:wallet (skipped without it):
// an HTLC claim and a refund pay the going rate, and are replaced (RBF) at the
// new going rate when it rises past them while they wait, the replacement
// re-sent on a retry; an HTLC funding under the going rate is lifted by CPFP,
// its txid kept; each is dropped once in a block.

func regtestChain(t *testing.T) *BitcoinChain {
	t.Helper()
	spec := os.Getenv("SEQDEX_BTC_REGTEST")
	if spec == "" {
		t.Skip("SEQDEX_BTC_REGTEST names no bitcoind regtest node")
	}
	p := strings.Split(spec, ":")
	if len(p) != 5 {
		t.Fatalf("SEQDEX_BTC_REGTEST=host:port:user:password:wallet, not %q", spec)
	}
	port, _ := strconv.Atoi(p[1])
	c := NewBitcoinChain(NewRPC(p[0], port, p[2], p[3]), p[4], &chaincfg.RegressionNetParams)
	KeepEvery = time.Hour // the test drives pass() itself
	return c
}

func mine(t *testing.T, c *BitcoinChain) {
	t.Helper()
	var a string
	if err := c.RPC().Call(&a, "getnewaddress"); err != nil {
		t.Fatal(err)
	}
	var h []string
	if err := c.RPC().Call(&h, "generatetoaddress", 1, a); err != nil {
		t.Fatal(err)
	}
}

func rateOf(t *testing.T, c *BitcoinChain, txid string) float64 {
	t.Helper()
	var e mempoolEntry
	if err := c.RPC().Call(&e, "getmempoolentry", txid); err != nil {
		t.Fatalf("%s not in the mempool: %v", txid, err)
	}
	return e.Fees.Base * 1e8 / float64(e.VSize)
}

func inPool(c *BitcoinChain, txid string) bool {
	var e mempoolEntry
	return c.RPC().Call(&e, "getmempoolentry", txid) == nil
}

// fundedHTLC funds an HTLC of 0.1 BTC and mines it, returning the backend and the leg.
func fundedHTLC(t *testing.T, c *BitcoinChain, locktime uint32) (*Swap, *bitcoinBTCBackend, *LegLock, *Key, *Key) {
	t.Helper()
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	sw := NewSwapBitcoin(c, nil, NewHashLock(secret))
	b := sw.btcBackend.(*bitcoinBTCBackend)
	claim, _ := NewKey()
	refund, _ := NewKey()
	script, err := b.HTLCScript(claim.PubKey(), refund.PubKey(), locktime)
	if err != nil {
		t.Fatal(err)
	}
	addr, _ := b.leg.P2SHAddress(script)
	var txid string
	if err := c.RPC().CallNamed(&txid, "sendtoaddress", map[string]interface{}{"address": addr, "amount": "0.1", "fee_rate": 5}); err != nil {
		t.Fatal(err)
	}
	mine(t, c)
	raw, _, err := c.RawTxAndConfirmations(txid)
	if err != nil {
		t.Fatal(err)
	}
	vout, amt, err := b.locateHTLCOutput(raw, script)
	if err != nil {
		t.Fatal(err)
	}
	return sw, b, &LegLock{Script: script, Funded: &FundedHTLC{TxID: txid, Vout: vout, Amount: amt}, Locktime: locktime}, claim, refund
}

func TestKeeperBumpsAClaimAndARefund(t *testing.T) {
	c := regtestChain(t)
	c.SetFeePolicy(FeePolicy{Floor: 5, Cap: 500})
	tip, _ := c.BlockCount()

	// The claim: the caller asks 1,000 sats; the going rate (5) asks more.
	sw, b, leg, claimKey, _ := fundedHTLC(t, c, uint32(tip)+100)
	tx1, err := sw.ClaimBTCLeg(leg, claimKey, 1000)
	if err != nil {
		t.Fatal(err)
	}
	r1 := rateOf(t, c, tx1)
	t.Logf("claim %s at %.2f sat/vB", tx1, r1)
	if r1 < 4.9 {
		t.Fatalf("the claim pays %.2f sat/vB, under the going rate 5", r1)
	}
	// The going rate rises past it: one pass replaces it.
	c.SetFeePolicy(FeePolicy{Floor: 25, Cap: 500})
	b.keeper().pass()
	b.keeper().mu.Lock()
	p := b.keeper().spends[leg.Funded.TxID+":"+strconv.Itoa(int(leg.Funded.Vout))+":claim"]
	b.keeper().mu.Unlock()
	if p == nil || p.txid == tx1 {
		t.Fatal("the claim was not replaced")
	}
	r2 := rateOf(t, c, p.txid)
	t.Logf("claim replaced by %s at %.2f sat/vB", p.txid, r2)
	if r2 < 24.9 || inPool(c, tx1) {
		t.Fatalf("the replacement pays %.2f sat/vB; the old claim in the pool: %v", r2, inPool(c, tx1))
	}
	// A retry re-sends the replacement, not the claim it replaced.
	tx3, err := sw.ClaimBTCLeg(leg, claimKey, 1000)
	if err != nil || tx3 != p.txid {
		t.Fatalf("a retry gave %s (%v), not the replacement %s", tx3, err, p.txid)
	}
	// In a block: dropped, and nothing left to settle.
	mine(t, c)
	b.keeper().pass()
	if s, _ := b.keeper().pending(); s != 0 {
		t.Fatalf("%d spends still kept after the block", s)
	}
	if err := SettleBTCSpends(time.Second, nil); err != nil {
		t.Fatal(err)
	}

	// The refund, once its locktime is reached.
	c.SetFeePolicy(FeePolicy{Floor: 5, Cap: 500})
	tip, _ = c.BlockCount()
	sw2, b2, leg2, _, refundKey := fundedHTLC(t, c, uint32(tip)+1)
	rf1, err := sw2.RefundBTCLeg(leg2, refundKey, leg2.Locktime, 1000)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("refund %s at %.2f sat/vB", rf1, rateOf(t, c, rf1))
	c.SetFeePolicy(FeePolicy{Floor: 40, Cap: 500})
	b2.keeper().pass()
	b2.keeper().mu.Lock()
	var rf2 string
	for _, q := range b2.keeper().spends {
		rf2 = q.txid
	}
	b2.keeper().mu.Unlock()
	if rf2 == rf1 || rateOf(t, c, rf2) < 39.9 {
		t.Fatalf("the refund was not replaced at the going rate: %s", rf2)
	}
	t.Logf("refund replaced by %s at %.2f sat/vB", rf2, rateOf(t, c, rf2))
	mine(t, c)
	if err := SettleBTCSpends(time.Second, nil); err != nil {
		t.Fatal(err)
	}
}

func TestKeeperLiftsAFundingByCPFP(t *testing.T) {
	c := regtestChain(t)
	c.SetFeePolicy(FeePolicy{Floor: 2, Cap: 500})
	var to, txid string
	_ = c.RPC().Call(&to, "getnewaddress")
	if err := c.RPC().CallNamed(&txid, "sendtoaddress", map[string]interface{}{"address": to, "amount": "1", "fee_rate": 2}); err != nil {
		t.Fatal(err)
	}
	k := newBTCKeeper(c)
	k.watchFunding(txid)
	c.SetFeePolicy(FeePolicy{Floor: 30, Cap: 500})
	k.pass()
	var e mempoolEntry
	if err := c.RPC().Call(&e, "getmempoolentry", txid); err != nil {
		t.Fatal(err)
	}
	pkg := e.Fees.Descendant * 1e8 / float64(e.DescendantSize)
	t.Logf("funding %s at %.2f sat/vB alone, %.2f with its child", txid, e.Fees.Base*1e8/float64(e.VSize), pkg)
	if pkg < 29.5 {
		t.Fatalf("the funding and its child pay %.2f sat/vB, under the going rate 30", pkg)
	}
	// The txid the other side holds is unchanged; in a block it is dropped.
	mine(t, c)
	k.pass()
	if _, f := k.pending(); f != 0 {
		t.Fatal("the funding is still kept after the block")
	}
}

// A wallet the node dropped is loaded again before a maker posts, and its spendable balance reads.
func TestEnsureWalletLoaded(t *testing.T) {
	c := regtestChain(t)
	var r struct{ Name string `json:"name"` }
	base := NewRPC(c.rpc.url[len("http://"):strings.LastIndex(c.rpc.url, ":")], func() int {
		p, _ := strconv.Atoi(c.rpc.url[strings.LastIndex(c.rpc.url, ":")+1:]); return p }(), c.rpc.user, c.rpc.pass)
	if err := base.Call(&r, "unloadwallet", c.rpc.wallet); err != nil {
		t.Fatalf("unload: %v", err)
	}
	if _, err := c.SpendableSats(); err == nil {
		t.Fatal("an unloaded wallet answered")
	}
	if err := c.EnsureWalletLoaded(); err != nil {
		t.Fatalf("not loaded again: %v", err)
	}
	if err := c.EnsureWalletLoaded(); err != nil {
		t.Fatalf("a loaded wallet: %v", err)
	}
	sats, err := c.SpendableSats()
	if err != nil || sats == 0 {
		t.Fatalf("spendable %d, %v", sats, err)
	}
	t.Logf("loaded again; spendable %d sats", sats)
}
