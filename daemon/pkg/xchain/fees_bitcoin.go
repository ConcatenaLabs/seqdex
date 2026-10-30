package xchain

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
)

// FeePolicy sets what a Bitcoin payment of the swap engine pays: the going rate
// of the live mempool, never below Floor and never above Cap (sat/vB).
//
// testnet4 shows why a fixed rate does not work: its miners often mine empty
// blocks while its public mempool queues at tens of sat/vB, and its nodes'
// estimatesmartfee has little to go on. A funding or a spend paid at 2 sat/vB
// there waits for hours, and an HTLC spend that waits past the other side's
// timelock loses the swap. So every rate is read live, and a payment still
// unconfirmed when the going rate has risen past it is bumped (see btcKeeper).
type FeePolicy struct {
	Floor float64 // sat/vB; the least any payment pays
	Cap   float64 // sat/vB; the most the going rate is taken to be
	// API, when set, is a mempool.space-style fees endpoint
	// (".../api/v1/fees"): its /recommended fastestFee and the median of its
	// next projected block (/mempool-blocks) count toward the going rate.
	API string
}

// DefaultFeePolicy is the policy for a chain: testnet4 pays at least 30 sat/vB
// and reads the public mempool (mempool.space) too; mainnet and regtest read
// only the node and pay at least 1 sat/vB.
func DefaultFeePolicy(params *chaincfg.Params) FeePolicy {
	if params != nil && params.Name == "testnet4" {
		return FeePolicy{Floor: 30, Cap: 500, API: "https://mempool.space/testnet4/api/v1/fees"}
	}
	return FeePolicy{Floor: 1, Cap: 500}
}

// SetFeePolicy replaces the chain's fee policy.
func (c *BitcoinChain) SetFeePolicy(p FeePolicy) { c.fees = p }

// FeePolicy returns the chain's fee policy.
func (c *BitcoinChain) FeePolicy() FeePolicy { return c.fees }

// GoingRate is the sat/vB a payment pays now: the most of the policy floor, an
// explicit SetFeeRate, the node's estimatesmartfee for the next two blocks, the
// rate that makes the next block of the node's own mempool, and the public
// mempool's rate (when the policy names one); capped. why says where it came
// from, for logs.
func (c *BitcoinChain) GoingRate() (rate float64, why string) {
	est := c.estimateRate()
	nb := c.nextBlockRate()
	pub := c.publicRate()
	r := math.Max(math.Max(c.fees.Floor, c.feeRateSatVb), math.Max(est, math.Max(nb, pub)))
	capped := ""
	if c.fees.Cap > 0 && r > c.fees.Cap {
		r, capped = c.fees.Cap, fmt.Sprintf("; capped at %g", c.fees.Cap)
	}
	return math.Ceil(r*10) / 10, fmt.Sprintf("floor %g, set %g, node estimate %.1f, node mempool next block %.1f, public mempool %.1f%s",
		c.fees.Floor, c.feeRateSatVb, est, nb, pub, capped)
}

func (c *BitcoinChain) estimateRate() float64 {
	var r struct {
		FeeRate float64 `json:"feerate"` // BTC/kvB
	}
	if err := c.rpc.Call(&r, "estimatesmartfee", 2, "CONSERVATIVE"); err != nil {
		return 0
	}
	return r.FeeRate * 1e5
}

// nextBlockRate is the rate one above the last transaction that still fits a
// block (950,000 vB) when the node's mempool is ranked by mining score; 0 when
// the whole mempool fits one block.
func (c *BitcoinChain) nextBlockRate() float64 {
	var m map[string]struct {
		VSize        int64 `json:"vsize"`
		AncestorSize int64 `json:"ancestorsize"`
		Fees         struct {
			Base     float64 `json:"base"`
			Ancestor float64 `json:"ancestor"`
		} `json:"fees"`
	}
	if err := c.rpc.Call(&m, "getrawmempool", true); err != nil {
		return 0
	}
	type e struct {
		rate float64
		size int64
	}
	es := make([]e, 0, len(m))
	for _, x := range m {
		if x.VSize <= 0 {
			continue
		}
		own := x.Fees.Base * 1e8 / float64(x.VSize)
		if x.AncestorSize > 0 {
			own = math.Min(own, x.Fees.Ancestor*1e8/float64(x.AncestorSize))
		}
		es = append(es, e{own, x.VSize})
	}
	sort.Slice(es, func(i, j int) bool { return es[i].rate > es[j].rate })
	var cum int64
	for _, x := range es {
		cum += x.size
		if cum >= 950000 {
			return x.rate + 1
		}
	}
	return 0
}

var feeHTTP = &http.Client{Timeout: 10 * time.Second}

func (c *BitcoinChain) publicRate() float64 {
	if c.fees.API == "" {
		return 0
	}
	get := func(path string, out interface{}) error {
		resp, err := feeHTTP.Get(c.fees.API + path)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%s: %s", path, resp.Status)
		}
		return json.NewDecoder(resp.Body).Decode(out)
	}
	var rec struct {
		Fastest float64 `json:"fastestFee"`
	}
	var blocks []struct {
		Median   float64   `json:"medianFee"`
		FeeRange []float64 `json:"feeRange"`
	}
	r := 0.0
	if get("/recommended", &rec) == nil {
		r = rec.Fastest
	}
	if get("/mempool-blocks", &blocks) == nil && len(blocks) > 0 {
		r = math.Max(r, blocks[0].Median)
		if fr := blocks[0].FeeRange; len(fr) > 0 {
			r = math.Max(r, fr[len(fr)/2])
		}
	}
	return r
}

// spendFee is what an HTLC spend of vsize vB pays: the caller's fee, raised to
// the going rate, and never more than half the output it spends (the drivers'
// own bound, xcSafeFee).
func spendFee(asked uint64, rate float64, vsize int, amount uint64) uint64 {
	fee := asked
	if f := uint64(math.Ceil(rate * float64(vsize))); f > fee {
		fee = f
	}
	if max := amount / 2; fee > max {
		fee = max
	}
	return fee
}
