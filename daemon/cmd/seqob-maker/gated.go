package main

// gated.go is `-mode gated`: a holder's resting offer to sell a restricted
// (OpenAMP) asset while offline (offer member 30, GatedOrderTerms). The maker
// moves the asset from its enclave into a gated order through the asset's
// policy server, waits for the placement to confirm, and posts the signed
// offer to the relay, which lists it until a minute before its expiry. Buyers
// fill it through the policy server, with the maker away. The placement is
// written to -xstate-dir/gated-<txid>.json; with -resume, every saved order
// whose expiry has passed is sent home to the maker's enclave.

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/thanhpk/randstr"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"github.com/aejkcs50/seqdex/daemon/pkg/restricted"
	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

type gatedMakerConfig struct {
	relay        string
	makerKey     *btcec.PrivateKey
	offerID      string
	seqRPCURL    string
	seqWallet    string
	stateDir     string
	policyServer string
	aid          string
	enclaveKey   *btcec.PrivateKey
	asset        string // display hex: the restricted asset sold
	assetB       string // display hex: the asset paid for it
	value        uint64 // atoms of asset placed
	want         uint64 // atoms of assetB for the whole
	minLot       uint64
	expiry       time.Duration // from now to the order's expiry
	feeAsset     string
	rfaPerVb     uint64
	resume       bool
}

// gatedState is what -xstate-dir keeps of a placed order.
type gatedState struct {
	Placed  *restricted.Placed
	OfferID string
	Refund  string `json:",omitempty"` // the refund's txid once sent home
	Ended   string `json:",omitempty"`
}

// gatedRate reduces want/value to the order's rate, so that
// rate_num * value == rate_den * want, as the relay requires.
func gatedRate(value, want uint64) (num, den uint64) {
	g := new(big.Int).GCD(nil, nil, new(big.Int).SetUint64(want), new(big.Int).SetUint64(value)).Uint64()
	return want / g, value / g
}

// buildGatedOffer is the signed-to-be offer for a placed order: TRADE_DIR_SELL
// of the restricted asset (the base) for assetB, resting until a minute before
// the expiry, when the policy server stops filling it.
func buildGatedOffer(pl *restricted.Placed, offerID, policyServer, aid string) *seqobv1.Offer {
	g := pl.Order
	t := pl.Terms
	want := t.Value * g.RateNum / g.RateDen
	gt := &seqobv1.GatedOrderTerms{
		CovenantTxid: pl.Txid, CovenantVout: pl.Vout,
		AssetA: hex.EncodeToString(g.AssetA[:]), AssetB: hex.EncodeToString(g.AssetB[:]),
		RateNum: g.RateNum, RateDen: g.RateDen, MakerProg: append([]byte{}, g.MakerProg...), MakerProgVer: 1,
		MinLot: g.MinLot, Expiry: g.Expiry, PolicyX: append([]byte{}, g.PolicyX[:]...),
		MakerEnclaveProg: append([]byte{}, g.MakerEnclave[:]...), PolicyServer: policyServer, MakerAid: aid,
	}
	if g.IssuerX != nil {
		gt.IssuerX = append([]byte{}, g.IssuerX[:]...)
	}
	asset := restricted.Display(g.AssetA)
	b := restricted.Display(g.AssetB)
	return &seqobv1.Offer{
		OfferId:       offerID,
		SchemaVersion: 1,
		Pair:          &seqobv1.AssetPair{BaseAsset: asset, QuoteAsset: b},
		TradeDir:      seqobv1.TradeDir_TRADE_DIR_SELL,
		OfferAsset:    asset, OfferAmount: t.Value,
		WantAsset: b, WantAmount: want,
		BaseAmount:    t.Value,
		MinFill:       g.MinLot,
		CreatedAtUnix: uint64(time.Now().Unix()),
		ExpiresAtUnix: uint64(offer.GatedDelistAt(gt)),
		FeeAssetHint:  t.FeeAsset,
		Settlement:    &seqobv1.Offer_GatedOrder{GatedOrder: gt},
	}
}

func writeGatedState(dir string, st *gatedState) error {
	b, err := json.MarshalIndent(st, "", " ")
	if err != nil {
		return err
	}
	p := filepath.Join(dir, "gated-"+st.Placed.Txid+".json")
	if err := os.WriteFile(p+".tmp", b, 0o600); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}

func postOffer(relay string, o *seqobv1.Offer) error {
	b, err := jsonMarshal.Marshal(o)
	if err != nil {
		return err
	}
	resp, err := http.Post(relay+"/v1/offers", "application/json", bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var msg bytes.Buffer
	_, _ = msg.ReadFrom(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("relay %d: %s", resp.StatusCode, strings.TrimSpace(msg.String()))
	}
	return nil
}

func runGatedMaker(cfg gatedMakerConfig) {
	if cfg.seqRPCURL == "" || cfg.policyServer == "" || cfg.aid == "" || cfg.enclaveKey == nil || cfg.feeAsset == "" {
		fatal("-mode gated requires -xseq-rpc, -policy-server, -openamp-aid, -openamp-key and -pot-asset (the fee asset)")
	}
	seqRPC, err := rpcFromURL(cfg.seqRPCURL)
	if err != nil {
		fatal("-xseq-rpc: %v", err)
	}
	party := &restricted.Party{Node: &restricted.Node{Chain: xchain.NewChain(seqRPC, cfg.seqWallet)},
		Policy: restricted.NewPolicy(cfg.policyServer), AID: cfg.aid, Key: cfg.enclaveKey, Asset: cfg.asset}
	if err := os.MkdirAll(cfg.stateDir, 0o700); err != nil {
		fatal("create -xstate-dir %s: %v", cfg.stateDir, err)
	}
	if cfg.resume {
		if resumeGated(cfg, party) > 0 {
			os.Exit(1)
		}
		return
	}
	if cfg.assetB == "" || offer.IsBTCSentinel(cfg.assetB) {
		fatal("-mode gated sells for an asset: -quote-asset names it (for bitcoin, use -mode restricted)")
	}
	if cfg.value == 0 || cfg.want == 0 {
		fatal("-base-amount and -quote-amount must be > 0")
	}
	minLot := cfg.minLot
	if minLot == 0 {
		minLot = cfg.value
	}
	num, den := gatedRate(cfg.value, cfg.want)
	expiry := uint32(party.Node.Now().Add(cfg.expiry).Unix())
	pl, err := party.PlaceGated(restricted.GatedTerms{AssetB: cfg.assetB, Value: cfg.value, RateNum: num, RateDen: den,
		MinLot: minLot, Expiry: expiry, FeeAsset: cfg.feeAsset}, cfg.rfaPerVb)
	if err != nil {
		fatal("not placed: %v", err)
	}
	st := &gatedState{Placed: pl, OfferID: orDefault(cfg.offerID, randstr.Hex(16))}
	if err := writeGatedState(cfg.stateDir, st); err != nil {
		fmt.Printf("state: %v (the order goes home after %d by -resume only with its state)\n", err, expiry)
	}
	fmt.Printf("placed %d %s in the gated order %s:%d, expiry %d\n", cfg.value, cfg.asset, pl.Txid, pl.Vout, expiry)
	// The relay lists a covenant order only once its coin is on the chain.
	for {
		if bh, err := party.Node.Chain.BlockHashOfTx(pl.Txid); err == nil && bh != "" {
			break
		}
		if party.Node.Now().Unix() >= offer.GatedDelistAt(&seqobv1.GatedOrderTerms{Expiry: expiry}) {
			fatal("the placement %s was not mined before the order stops filling; -resume sends it home after %d", pl.Txid, expiry)
		}
		time.Sleep(5 * time.Second)
	}
	o := buildGatedOffer(pl, st.OfferID, cfg.policyServer, cfg.aid)
	if err := offer.SignOffer(o, cfg.makerKey); err != nil {
		fatal("sign offer: %v", err)
	}
	if err := postOffer(cfg.relay, o); err != nil {
		fatal("post: %v (the order rests on the chain; -resume sends it home after %d)", err, expiry)
	}
	fmt.Printf("posted gated offer %s: %d %s for %d %s, listed until %d\n", o.GetOfferId(), cfg.value, cfg.asset,
		o.GetWantAmount(), cfg.assetB, o.GetExpiresAtUnix())
}

// resumeGated sends home every saved order whose expiry has passed, and
// returns how many still wait for theirs.
func resumeGated(cfg gatedMakerConfig, party *restricted.Party) int {
	paths, err := filepath.Glob(filepath.Join(cfg.stateDir, "gated-*.json"))
	if err != nil {
		fatal("-xstate-dir %s: %v", cfg.stateDir, err)
	}
	owed := 0
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			fmt.Printf("resume %s: %v\n", p, err)
			owed++
			continue
		}
		var st gatedState
		if err := json.Unmarshal(b, &st); err != nil || st.Placed == nil {
			fmt.Printf("resume %s: the state does not read: %v\n", p, err)
			owed++
			continue
		}
		if st.Ended != "" {
			continue
		}
		pp := *party
		pp.Asset = restricted.Display(st.Placed.Order.AssetA)
		txid, err := pp.RefundGated(st.Placed)
		switch {
		case err != nil:
			fmt.Printf("resume %s: still owed: %v\n", st.Placed.Txid, err)
			owed++
			continue
		case txid == "":
			st.Ended = "filled whole, or already home"
		default:
			st.Refund, st.Ended = txid, "sent home after the expiry"
		}
		_ = writeGatedState(cfg.stateDir, &st)
		fmt.Printf("resume %s: %s %s\n", st.Placed.Txid, st.Ended, st.Refund)
	}
	fmt.Printf("resume: %d gated order(s) still wait for their expiry\n", owed)
	return owed
}
