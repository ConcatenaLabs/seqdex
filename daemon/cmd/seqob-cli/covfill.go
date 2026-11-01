package main

// covfill: the taker for resting passive-CLOB covenant orders.
//
// A covenant offer is an on-chain UTXO, under the tapscript FILL leaf or the
// Simplicity order of the template seqdex/order, that ANYONE can fill by
// assembling the fill spend that pays the maker its want (asset B, at the
// covenant's own ceil price) and takes the offered asset A — no maker liveness,
// no courier session, no signature from anyone but the taker's own wallet. This
// command fetches each offer from the relay, verifies the maker signature AND
// the on-chain covenant UTXO (trustless spk re-derivation, unspent, funded as
// advertised), then drives internal/seqob/covfill: assemble, wallet-sign,
// testmempoolaccept-gate, broadcast. Several offers (comma-separated ids, keys
// and amounts, in the same order) are filled in one transaction, the n-th at
// input n-1: its maker's credit at output 2(n-1), its rest at 2(n-1)+1.
//
// A covenant fill is ONE atomic tx: on failure nothing moved (no refund state,
// no resume command — unlike the HTLC takers there is nothing to recover).
//
//	seqob-cli covfill -relay <r> -offer-id <id[,id...]> -maker-pubkey <pk[,pk...]>
//	  -amount <asset-A atoms | 0 = whole covenant>[,...]
//	  -seq-rpc http://user:pass@host:port -seq-wallet <w>
//	  [-base <hex> -quote <hex>]   (the offer's market; scanned from /v1/markets when omitted)
//	  [-fee-asset <display hex>]   (default: the asset the fill pays in, if the node accepts it for
//	                                fees; else the accepted asset whose largest coin in the wallet
//	                                covers the most fees; refused if the node accepts none)
//	  [-spend-fee <atoms>]         (explicit network fee in the fee asset's own atoms)

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/bridge"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/covfill"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

func cmdCovFill(args []string) {
	fs := newFlagSet("covfill")
	relay := fs.String("relay", "http://127.0.0.1:9955", "relay base URL")
	base := fs.String("base", "", "market base asset (optional; with -quote skips the market scan)")
	quote := fs.String("quote", "", "market quote asset (optional)")
	offerIDs := fs.String("offer-id", "", "offer id(s) to fill, comma-separated (required)")
	makerPubs := fs.String("maker-pubkey", "", "maker pubkey of each offer, comma-separated in the same order (required)")
	amounts := fs.String("amount", "0", "asset-A atoms to take of each offer, comma-separated (0 = the whole covenant; a dust remainder auto-shrinks)")
	seqRPCURL := envString(fs, "seq-rpc", "SEQOB_SEQ_RPC", "Sequentia node RPC URL http://user:pass@host:port (required)")
	seqWallet := fs.String("seq-wallet", "", "Sequentia node wallet funding the fill (pays asset B + the fee, receives asset A) (required)")
	feeAsset := fs.String("fee-asset", "", "fee asset (display hex; default: the asset the fill pays in, if the node accepts it for fees, else the accepted asset whose largest coin in the wallet covers the most fees)")
	spendFee := fs.Uint64("spend-fee", 2000, "explicit network fee, in the fee asset's own atoms")
	allowCancellable := fs.Bool("allow-maker-cancellable", false, "fill a non-NUMS-internal-key covenant (maker key-path cancel/rug risk); default: refuse")
	maxPay := fs.Uint64("max-pay", 0, "refuse if a covenant demands more than this many asset-B atoms (0 = derive each from the offer's own advertised price)")
	_ = fs.Parse(args)

	if *offerIDs == "" || *makerPubs == "" || *seqRPCURL == "" || *seqWallet == "" {
		fatal("covfill requires -offer-id, -maker-pubkey, -seq-rpc and -seq-wallet")
	}
	ids, pubs := strings.Split(*offerIDs, ","), strings.Split(*makerPubs, ",")
	amts := strings.Split(*amounts, ",")
	if len(amts) == 1 && len(ids) > 1 {
		for len(amts) < len(ids) {
			amts = append(amts, amts[0])
		}
	}
	if len(pubs) != len(ids) || len(amts) != len(ids) {
		fatal("-offer-id, -maker-pubkey and -amount must list the same number of offers")
	}

	var takes []covfill.Take
	for i, id := range ids {
		amount, err := strconv.ParseUint(strings.TrimSpace(amts[i]), 10, 64)
		if err != nil {
			fatal("-amount %q: %v", amts[i], err)
		}
		target := findOffer(*relay, *base, *quote, id, pubs[i])
		if target == nil {
			fatal("offer %s by %s not found on %s", id, short(pubs[i]), *relay)
		}
		// The relay is untrusted: verify the maker's signature over the served offer
		// before acting on it (the same gate every taker command applies).
		if err := offer.VerifyOffer(target); err != nil {
			fatal("offer %s failed maker signature verification (refusing to fill): %v", id, err)
		}
		ceiling := *maxPay
		if st := target.GetSimplicityCovenant(); st != nil {
			// A Simplicity order: its terms checked and its template the one this
			// build pins; FillCovenant re-derives its script and checks the chain.
			so, err := offer.SimOrderOf(st)
			if err != nil {
				fatal("simplicity covenant terms: %v", err)
			}
			fmt.Printf("simplicity covenant offer %s: fills at rate %d/%d (min_lot %d), resting at %s:%d, taken at input %d\n",
				id, so.RateNum, so.RateDen, so.MinLot, st.GetCovenantTxid(), st.GetCovenantVout(), i)
			if ceiling == 0 {
				ceiling = offerCeiling(target, amount)
			}
			takes = append(takes, covfill.Take{Sim: &so, Txid: st.GetCovenantTxid(), Vout: st.GetCovenantVout(), TakeAtoms: amount, MaxPayB: ceiling})
			continue
		}
		ct := target.GetCovenant()
		if ct == nil {
			fatal("offer %s is not a covenant offer (settlement family %T)", id, target.GetSettlement())
		}
		order, err := bridge.OrderFromTerms(ct)
		if err != nil {
			fatal("covenant terms: %v", err)
		}
		op := bridge.OutpointFromTerms(ct)
		fmt.Printf("covenant offer %s: fills at rate %d/%d (min_lot %d), resting at %s:%d, taken at input %d\n",
			id, ct.GetRateNum(), ct.GetRateDen(), ct.GetMinLot(), op.Txid, op.Vout, i)
		if ceiling == 0 {
			// The offer's own terms are the price the taker agreed to: a full fill of
			// offer_amount of asset A is worth want_amount of asset B, pro rata for a
			// partial, rounded up as the leaf rounds. The covenant's baked-in rate must
			// not demand more than that.
			ceiling = offerCeiling(target, amount)
		}
		takes = append(takes, covfill.Take{Order: order, Txid: op.Txid, Vout: op.Vout, TakeAtoms: amount, MaxPayB: ceiling})
	}

	seqRPC, err := xliftRPCFromURL(*seqRPCURL)
	if err != nil {
		fatal("-seq-rpc: %v", err)
	}
	chain := xchain.NewChain(seqRPC, *seqWallet)

	fee := *feeAsset
	if fee == "" {
		// The open fee market's wallet rule: no asset is the default. The asset the
		// fill moves out of the taker's wallet is the first take's payment asset.
		var moved string
		sold := map[string]bool{}
		for i, t := range takes {
			leaf := t.Order.Sells()
			paysIn := t.Order.PaysIn()
			if t.Sim != nil {
				leaf, paysIn = t.Sim.Sells(), t.Sim.PaysIn()
			}
			sold[displayHex32(leaf)] = true
			if i == 0 {
				moved = displayHex32(paysIn)
			}
		}
		picked, perr := covfill.FeeAssetFor(chain.RPC(), moved, sold)
		if perr != nil {
			fatal("%v", perr)
		}
		fee = picked
		why := "the asset the fill pays in, which the node accepts for fees"
		if fee != moved {
			why = "the node does not accept the asset the fill pays in for fees; this is the accepted asset whose largest coin in the wallet covers the most fees"
		}
		fmt.Printf("fee asset %s: %s\n", fee, why)
	}

	res, err := covfill.FillCovenant(covfill.Params{
		Takes:                 takes,
		FeeAtoms:              *spendFee,
		FeeAssetDisplay:       fee,
		Node:                  chain.RPC(),
		AllowMakerCancellable: *allowCancellable,
	})
	if err != nil {
		fatal("covenant fill: %v", err)
	}

	for _, f := range res.Fills {
		fmt.Printf("input %d: filled %d asset-A atoms of %s:%d%s, paid the maker %d asset-B atoms at output %d (remainder %d at output %d)\n",
			f.InputIndex, f.Filled, f.Txid, f.Vout, partialNote(f.Filled, f.Locked), f.PaidB, f.CreditIndex, f.Remainder, f.RemainderIndex)
	}
	fmt.Printf("SWAP SETTLED: txid %s\n", res.Txid)
}

// offerCeiling is the most asset B a fill of `take` asset-A atoms (0 = all) may
// credit the maker under the OFFER's advertised price: ceil(take * want / offer).
// The covenant's asset A is the offer_asset and asset B the want_asset, so the
// advertised ratio and the baked-in rate price the same pair.
func offerCeiling(o *seqobv1.Offer, take uint64) uint64 {
	offerAmt, wantAmt := o.GetOfferAmount(), o.GetWantAmount()
	if take == 0 || take >= offerAmt {
		return wantAmt
	}
	return covenant.CeilPrice(take, wantAmt, offerAmt)
}

// findOffer locates the target offer: directly in one market's book when
// base/quote are given, else by scanning every market the relay lists.
func findOffer(relay, base, quote, offerID, makerPub string) *seqobv1.Offer {
	lookup := func(b, q string) *seqobv1.Offer {
		var book seqobv1.PublicBook
		if err := getJSON(fmt.Sprintf("%s/v1/market/%s/%s/orderbook", relay, b, q), &book); err != nil {
			return nil
		}
		for _, o := range book.GetOffers() {
			if o.GetOfferId() == offerID && o.GetMakerPubkey() == makerPub {
				return o
			}
		}
		return nil
	}
	if base != "" && quote != "" {
		return lookup(base, quote)
	}
	var ml seqobv1.MarketList
	if err := getJSON(relay+"/v1/markets", &ml); err != nil {
		fatal("list markets on %s: %v (or pass -base/-quote)", relay, err)
	}
	for _, m := range ml.GetMarkets() {
		if o := lookup(m.GetPair().GetBaseAsset(), m.GetPair().GetQuoteAsset()); o != nil {
			return o
		}
	}
	return nil
}

// displayHex32 is an internal-order asset id in display (RPC) order.
func displayHex32(internal [32]byte) string {
	b := make([]byte, 32)
	for i := range b {
		b[i] = internal[31-i]
	}
	return hex.EncodeToString(b)
}
