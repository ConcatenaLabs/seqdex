// Package validator validates SeqOB offers before they enter the book.
//
// Checks: signature, schema, amounts > 0, known assets, trade-direction/asset
// consistency, a mandatory non-absurd expiry (short self-expiry so a suppressed
// cancel self-heals — review M3), and per-maker_pubkey + per-IP rate limits.
//
// It does NOT enforce a min_anchor_depth floor: per the project policy override
// the DEX is 0-conf tolerant and min_anchor_depth is a maker-only dial that
// defaults to 0. The liveness probe is an interface with a no-op default; a
// relay with a node reads the chain for covenant orders (watcher.SubmitProbe),
// which also sets the size a covenant order resting at its remainder is listed
// at. Interactive offers are not probed (confidential balances make a precise
// probe impossible; atomic co-signing is the real safety net).
package validator

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
	"github.com/btcsuite/btcd/btcec/v2"
	"google.golang.org/protobuf/proto"
)

// MaxOfferBytes caps the wire size of one offer. An offer is a few hundred bytes of
// signed terms; the relay stores and re-serves every accepted one verbatim, so an
// unbounded offer_id or hint list would let one submitter fill relay memory and
// every subscriber's snapshot at 120 offers per minute per IP.
const MaxOfferBytes = 16 << 10

// ErrReplay signals that the submitted offer is a byte-identical replay of an
// offer already resting in the book (same maker_pubkey + offer_id + maker_sig):
// a no-op. It is NOT a validation failure; the caller should drop the submission
// silently (re-acking the live order status) WITHOUT treating it as an error.
// Crucially, an ErrReplay submission consumes NO per-maker_pubkey rate budget,
// which is the whole point: see RestingOffers and ValidateOffer.
var ErrReplay = errors.New("offer is an exact replay of an already-resting offer")

// RestingOffers lets the validator consult the live order book so a byte-identical
// replay of an already-resting offer is dropped WITHOUT charging the per-maker
// pubkey rate budget. The offerstore.Store satisfies this interface. If no book
// is wired (SetBook never called), the replay gate is skipped.
type RestingOffers interface {
	// RestingMakerSig returns the maker_sig of the offer currently resting under
	// (makerPubkey, offerID), or ok=false if none rests.
	RestingMakerSig(makerPubkey, offerID string) (sig []byte, ok bool)
}

// LivenessProbe checks (best-effort, read-only) that an offer is backed as it
// claims. The relay's covenant probe (watcher.SubmitProbe) reads the chain: a
// covenant order is admitted only if its outpoint is unspent under the order's
// own program and holds between the order's min_lot and its signed
// offer_amount.
//
// listed is the base atoms the order is listed at: 0 means its signed size (an
// offer that is not a covenant, or no chain evidence either way); a covenant
// order whose outpoint holds less than offer_amount (its remainder after a part
// fill) is listed at what the outpoint holds, never more than the maker signed.
type LivenessProbe interface {
	CheckOffer(ctx context.Context, o *seqobv1.Offer) (listed uint64, err error)
}

// NoopLivenessProbe is the probe of a relay without a node: it accepts every
// offer at its signed size.
type NoopLivenessProbe struct{}

// CheckOffer always admits the offer at its signed size.
func (NoopLivenessProbe) CheckOffer(context.Context, *seqobv1.Offer) (uint64, error) { return 0, nil }

// Config tunes validation.
type Config struct {
	// SchemaVersion the relay accepts (default 1).
	SchemaVersion uint32
	// KnownAssets, if non-empty, restricts offers to these asset ids (hex). Empty
	// = accept any asset (registry wiring is deferred).
	KnownAssets map[string]bool
	// MinExpiry / MaxExpiry bound how soon / far an offer may expire from now.
	// Expiry is mandatory: an offer with no future expiry is rejected.
	MinExpiry time.Duration
	MaxExpiry time.Duration
	// Rate limits (sliding 1-minute window).
	MaxOffersPerMinPerPubkey int
	MaxOffersPerMinPerIP     int
	// Now is the clock (default time.Now).
	Now func() time.Time
}

// DefaultConfig returns sane Phase-1 defaults.
func DefaultConfig() Config {
	return Config{
		SchemaVersion:            1,
		MinExpiry:                30 * time.Second,
		MaxExpiry:                7 * 24 * time.Hour,
		MaxOffersPerMinPerPubkey: 60,
		MaxOffersPerMinPerIP:     120,
		Now:                      time.Now,
	}
}

// Validator validates offers and enforces rate limits.
type Validator struct {
	cfg     Config
	probe   LivenessProbe
	book    RestingOffers
	mu      sync.Mutex
	pubHits map[string][]time.Time
	ipHits  map[string][]time.Time
	// lastSweep is when the rate maps were last purged of keys whose window has
	// emptied. A key is pruned only when it is hit again, so keys that are never
	// seen twice (fresh pubkeys are free to mint) would otherwise accumulate forever.
	lastSweep time.Time
}

// SetBook wires the live order book so ValidateOffer can recognize a byte-identical
// replay of an already-resting offer and decline to charge it the per-maker_pubkey
// rate budget. Call once at wiring time (e.g. api.New); b may be nil to disable.
func (v *Validator) SetBook(b RestingOffers) { v.book = b }

// New returns a Validator. If probe is nil, NoopLivenessProbe is used.
func New(cfg Config, probe LivenessProbe) *Validator {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.SchemaVersion == 0 {
		cfg.SchemaVersion = 1
	}
	if probe == nil {
		probe = NoopLivenessProbe{}
	}
	return &Validator{
		cfg:     cfg,
		probe:   probe,
		pubHits: make(map[string][]time.Time),
		ipHits:  make(map[string][]time.Time),
	}
}

// ValidateOffer runs all structural/semantic checks and rate limits. ip may be
// empty (e.g. for a local CLI); IP rate limiting is then skipped.
//
// Order matters for rate limiting (review: per-maker_pubkey griefing). The
// per-IP limit is charged FIRST, before the (relatively expensive) signature
// verification, so a flood from one IP is throttled cheaply. The maker signature
// is then verified, and ONLY after it succeeds is the per-maker_pubkey budget
// charged: a maker_pubkey is attacker-replayable PUBLIC data, so a forged offer
// bearing a victim's maker_pubkey must never consume the victim's budget. The
// genuine maker (the only party able to produce a valid maker_sig) is therefore
// the only one who spends its own per-pubkey budget.
func (v *Validator) ValidateOffer(ctx context.Context, o *seqobv1.Offer, ip string) error {
	_, err := v.Admit(ctx, o, ip)
	return err
}

// Admit is ValidateOffer that also returns the base atoms the offer is listed
// at (see LivenessProbe): 0 for its signed size, less for a covenant order
// resting at its remainder. The relay stores the offer at that size.
func (v *Validator) Admit(ctx context.Context, o *seqobv1.Offer, ip string) (listed uint64, err error) {
	if err := v.checkIPRate(ip); err != nil {
		return 0, err
	}
	if err := v.checkOffer(o); err != nil {
		return 0, err
	}
	// Replay-griefing defense: an attacker can harvest a victim maker's GENUINE
	// signed offer from the public book and replay it here. It passes VerifyOffer
	// (the signature is real), so without this gate it would consume the victim's
	// per-maker_pubkey budget and could lock the victim out. If an offer with this
	// (maker_pubkey, offer_id) is already resting with a byte-identical maker_sig
	// -- i.e. identical signed content, since the signature is a deterministic,
	// collision-resistant function of the whole signed payload -- the submission is
	// a no-op replay: return ErrReplay WITHOUT charging the per-pubkey budget. A new
	// offer_id, or a genuinely changed (re-signed) offer_edit, has a different
	// maker_sig and falls through to consume budget and be stored.
	if v.book != nil {
		if sig, ok := v.book.RestingMakerSig(o.GetMakerPubkey(), o.GetOfferId()); ok && bytes.Equal(sig, o.GetMakerSig()) {
			return 0, ErrReplay
		}
	}
	// Charge the per-maker_pubkey budget only now that maker_sig is verified and the
	// offer is a genuinely new or changed resting offer (not a no-op replay).
	if err := v.checkPubkeyRate(o.GetMakerPubkey()); err != nil {
		return 0, err
	}
	return v.liveness(ctx, o)
}

// Readmit checks a covenant order a relay kept across its restart before
// listing it again: every check on the offer itself and the liveness probe, as
// Admit, but no rate limit and no replay gate, which meter what clients post
// rather than what the relay already held. It returns the size to list it at.
func (v *Validator) Readmit(ctx context.Context, o *seqobv1.Offer) (listed uint64, err error) {
	if err := v.checkOffer(o); err != nil {
		return 0, err
	}
	return v.liveness(ctx, o)
}

func (v *Validator) liveness(ctx context.Context, o *seqobv1.Offer) (uint64, error) {
	listed, err := v.probe.CheckOffer(ctx, o)
	if err != nil {
		return 0, fmt.Errorf("liveness: %w", err)
	}
	return listed, nil
}

// checkOffer is every check on the offer itself: its size, the form of its
// key, its signature, schema, terms and expiry.
func (v *Validator) checkOffer(o *seqobv1.Offer) error {
	if n := proto.Size(o); n > MaxOfferBytes {
		return fmt.Errorf("offer too large (%d bytes, max %d)", n, MaxOfferBytes)
	}
	// The pubkey is part of the signed bytes and is the book's key string, so it
	// cannot be normalised here; both signers emit lowercase, and a mixed-case twin
	// would be a second book entry for the same key.
	if pk := o.GetMakerPubkey(); pk != strings.ToLower(pk) {
		return fmt.Errorf("maker_pubkey must be lowercase hex")
	}
	if err := offer.VerifyOffer(o); err != nil {
		return fmt.Errorf("signature: %w", err)
	}
	if o.GetSchemaVersion() != v.cfg.SchemaVersion {
		return fmt.Errorf("unsupported schema_version %d (want %d)", o.GetSchemaVersion(), v.cfg.SchemaVersion)
	}
	if err := v.checkTerms(o); err != nil {
		return err
	}
	return v.checkExpiry(o)
}

func (v *Validator) checkTerms(o *seqobv1.Offer) error {
	p := o.GetPair()
	if p == nil || p.GetBaseAsset() == "" || p.GetQuoteAsset() == "" {
		return fmt.Errorf("offer missing pair assets")
	}
	if p.GetBaseAsset() == p.GetQuoteAsset() {
		return fmt.Errorf("base and quote asset are identical")
	}
	if o.GetBaseAmount() == 0 || o.GetOfferAmount() == 0 || o.GetWantAmount() == 0 {
		return fmt.Errorf("amounts must be > 0")
	}
	if o.GetOfferAsset() == "" || o.GetWantAsset() == "" {
		return fmt.Errorf("offer/want asset missing")
	}
	// Direction/asset consistency: a SELL gives base and wants quote (and
	// offer_amount is the base side); a BUY gives quote and wants base (and
	// want_amount is the base side).
	switch o.GetTradeDir() {
	case seqobv1.TradeDir_TRADE_DIR_SELL:
		if o.GetOfferAsset() != p.GetBaseAsset() || o.GetWantAsset() != p.GetQuoteAsset() {
			return fmt.Errorf("SELL must offer base for quote")
		}
		if o.GetOfferAmount() != o.GetBaseAmount() {
			return fmt.Errorf("SELL offer_amount must equal base_amount")
		}
	case seqobv1.TradeDir_TRADE_DIR_BUY:
		if o.GetOfferAsset() != p.GetQuoteAsset() || o.GetWantAsset() != p.GetBaseAsset() {
			return fmt.Errorf("BUY must offer quote for base")
		}
		if o.GetWantAmount() != o.GetBaseAmount() {
			return fmt.Errorf("BUY want_amount must equal base_amount")
		}
	default:
		return fmt.Errorf("unspecified trade_dir")
	}
	if o.GetMinFill() > o.GetBaseAmount() {
		return fmt.Errorf("min_fill exceeds base_amount")
	}
	if len(v.cfg.KnownAssets) > 0 {
		for _, a := range []string{p.GetBaseAsset(), p.GetQuoteAsset(), o.GetOfferAsset(), o.GetWantAsset()} {
			if offer.IsBTCSentinel(a) {
				continue // the BTC sentinel is the cross-chain parent-chain placeholder, not a Sequentia asset
			}
			if !v.cfg.KnownAssets[strings.ToLower(a)] && !v.cfg.KnownAssets[a] {
				return fmt.Errorf("unknown asset %q", a)
			}
		}
	}
	if o.GetConfidential() {
		if err := v.checkConfidential(o); err != nil {
			return err
		}
	}
	if o.GetCrossChain() != nil {
		return v.checkCrossChain(o)
	}
	if o.GetLightning() != nil {
		return v.checkLightning(o)
	}
	if o.GetCovenant() != nil {
		return checkCovenant(o)
	}
	if o.GetSimplicityCovenant() != nil {
		return checkSimplicityCovenant(o)
	}
	return nil
}

// checkSimplicityCovenant is checkCovenant for an order under the Simplicity
// template seqdex/order: the terms well formed and of the template this relay
// pins (offer.SimOrderOf), tied to the price and the assets the offer
// advertises, and an expiry set. Its fill compares the price on 128-bit
// products, so no arithmetic bound applies; the chain probe then checks the
// outpoint pays the program the terms make and holds between min_lot and
// offer_amount.
func checkSimplicityCovenant(o *seqobv1.Offer) error {
	st := o.GetSimplicityCovenant()
	if b, err := hex.DecodeString(st.GetCovenantTxid()); err != nil || len(b) != 32 {
		return fmt.Errorf("covenant_txid must be 32-byte hex")
	}
	so, err := offer.SimOrderOf(st)
	if err != nil {
		return fmt.Errorf("simplicity covenant: %w", err)
	}
	if st.GetExpiryLocktime() == 0 {
		return fmt.Errorf("simplicity covenant expiry_locktime must be set: an order its refund key can take back at any time is not a resting order")
	}
	if o.GetTradeDir() != seqobv1.TradeDir_TRADE_DIR_SELL {
		return fmt.Errorf("covenant offers must be TRADE_DIR_SELL (asset A is the offered base)")
	}
	if so.MinLot > o.GetOfferAmount() {
		return fmt.Errorf("covenant min_lot %d exceeds the locked amount %d", so.MinLot, o.GetOfferAmount())
	}
	lhs := new(big.Int).Mul(new(big.Int).SetUint64(so.RateNum), new(big.Int).SetUint64(o.GetOfferAmount()))
	rhs := new(big.Int).Mul(new(big.Int).SetUint64(so.RateDen), new(big.Int).SetUint64(o.GetWantAmount()))
	if lhs.Cmp(rhs) != 0 {
		return fmt.Errorf("covenant rate %d/%d does not equal the offer's want/offer %d/%d", so.RateNum, so.RateDen, o.GetWantAmount(), o.GetOfferAmount())
	}
	if a, _ := internalToDisplay(st.GetAssetA()); a != strings.ToLower(o.GetOfferAsset()) {
		return fmt.Errorf("covenant asset_a %s is not the offer_asset %s", a, o.GetOfferAsset())
	}
	if b, _ := internalToDisplay(st.GetAssetB()); b != strings.ToLower(o.GetWantAsset()) {
		return fmt.Errorf("covenant asset_b %s is not the want_asset %s", b, o.GetWantAsset())
	}
	return nil
}

// checkCovenant ties the covenant's baked-in terms to the price the offer
// advertises. Both are under the maker's signature, but takers price from the
// offer fields and the chain enforces the terms: a covenant whose rate demands
// more than want/offer would be paid whatever the leaf requires, and one whose
// arithmetic overflows the leaf's signed 64-bit OP_MUL64 can never be filled at
// all (the locked asset waits for REFUND). The relay refuses both up front.
//
// Asset A is the offer_asset and asset B the want_asset, in internal byte order;
// that pairing is checked only when both sides are real 32-byte ids (fixtures use
// placeholder names).
func checkCovenant(o *seqobv1.Offer) error {
	ct := o.GetCovenant()
	// Structural bounds first, so a malformed covenant never reaches the chain
	// watcher (which would spend a pass of RPCs on it every interval).
	if b, err := hex.DecodeString(ct.GetCovenantTxid()); err != nil || len(b) != 32 {
		return fmt.Errorf("covenant_txid must be 32-byte hex")
	}
	for name, v := range map[string]string{"asset_a": ct.GetAssetA(), "asset_b": ct.GetAssetB()} {
		if b, err := hex.DecodeString(v); err != nil || (len(b) != 32 && len(v) != 2) {
			// 2-char placeholders are allowed only alongside non-hex pair names (fixtures).
			if _, ok := asset32(o.GetOfferAsset()); ok || len(v) != 2 {
				return fmt.Errorf("covenant %s must be 32-byte hex", name)
			}
		}
	}
	if len(ct.GetMakerProg()) != 32 || ct.GetMakerProgVer() != 1 {
		return fmt.Errorf("covenant maker_prog must be a 32-byte v1 program")
	}
	if len(ct.GetMakerX()) != 32 {
		return fmt.Errorf("covenant maker_x must be 32 bytes")
	}
	if ik := ct.GetInternalKey(); len(ik) != 0 && len(ik) != 32 {
		return fmt.Errorf("covenant internal_key must be 32 bytes when present")
	}
	// Any internal key but NUMS is a key path: whoever holds it takes the order
	// back at any moment, from under a taker who is filling it. Takers refuse such
	// an order (VerifyAgainstSPK), so the relay does not list one. A Simplicity
	// order (SimplicityCovenantTerms) carries no internal key: its terms derive
	// the NUMS-keyed output, which the probe requires the outpoint to pay.
	if ik := ct.GetInternalKey(); len(ik) == 32 && !bytes.Equal(ik, covenant.NUMS[:]) {
		return fmt.Errorf("covenant internal_key %x is not the NUMS point: it is a key path that takes the order back from under any taker, so the relay lists only NUMS-keyed orders", ik)
	}
	if ct.GetExpiryLocktime() == 0 {
		return fmt.Errorf("covenant expiry_locktime must be set")
	}
	if ct.GetRateNum() < 1 || ct.GetRateDen() < 1 {
		return fmt.Errorf("covenant rate %d/%d: numerator and denominator must be >= 1", ct.GetRateNum(), ct.GetRateDen())
	}
	if err := (covenant.Order{RateNum: ct.GetRateNum(), RateDen: ct.GetRateDen(), MinLot: ct.GetMinLot()}).CheckTerms(); err != nil {
		return fmt.Errorf("covenant: %w", err)
	}
	if ct.GetMinLot() < 1 {
		return fmt.Errorf("covenant min_lot must be >= 1")
	}
	// Only the SELL orientation (asset A = the offered base) is served end to end:
	// the matcher prices a resting BUY covenant in the wrong unit and the crosser's
	// overlap clamp compares its min_lot in the wrong asset. Nothing posts one; refuse
	// rather than rest an order every downstream step would mis-size.
	if o.GetTradeDir() != seqobv1.TradeDir_TRADE_DIR_SELL {
		return fmt.Errorf("covenant offers must be TRADE_DIR_SELL (asset A is the offered base)")
	}
	if ct.GetMinLot() > o.GetOfferAmount() {
		return fmt.Errorf("covenant min_lot %d exceeds the locked amount %d", ct.GetMinLot(), o.GetOfferAmount())
	}
	// rate_num/rate_den == want_amount/offer_amount exactly, cross-multiplied in
	// big.Int: the leaf prices filled*num/den, and a full fill must cost want_amount.
	lhs := new(big.Int).Mul(new(big.Int).SetUint64(ct.GetRateNum()), new(big.Int).SetUint64(o.GetOfferAmount()))
	rhs := new(big.Int).Mul(new(big.Int).SetUint64(ct.GetRateDen()), new(big.Int).SetUint64(o.GetWantAmount()))
	if lhs.Cmp(rhs) != 0 {
		return fmt.Errorf("covenant rate %d/%d does not equal the offer's want/offer %d/%d", ct.GetRateNum(), ct.GetRateDen(), o.GetWantAmount(), o.GetOfferAmount())
	}
	order := covenant.Order{RateNum: ct.GetRateNum(), RateDen: ct.GetRateDen()}
	if err := order.CheckArithmetic(o.GetOfferAmount()); err != nil {
		return fmt.Errorf("covenant: %w", err)
	}
	if a, ok := internalToDisplay(ct.GetAssetA()); ok {
		if off, ok := asset32(o.GetOfferAsset()); ok && a != off {
			return fmt.Errorf("covenant asset_a %s is not the offer_asset %s", a, off)
		}
	}
	if b, ok := internalToDisplay(ct.GetAssetB()); ok {
		if want, ok := asset32(o.GetWantAsset()); ok && b != want {
			return fmt.Errorf("covenant asset_b %s is not the want_asset %s", b, want)
		}
	}
	return nil
}

// asset32 normalises a 32-byte display-order asset id; ok=false for anything else.
func asset32(s string) (string, bool) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return "", false
	}
	return strings.ToLower(s), true
}

// internalToDisplay converts a 32-byte internal-order asset id to display order.
func internalToDisplay(s string) (string, bool) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return "", false
	}
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return hex.EncodeToString(b), true
}

// checkConfidential enforces the blinded-book invariants on an offer flagged
// confidential (Confidential-book requirement 1). A confidential offer:
//   - MUST settle via the interactive SameChainTerms co-sign rail. The covenant
//     rail introspects EXPLICIT on-chain amounts (tapscript 0xc4) and is
//     CT-incompatible; cross-chain / Lightning legs touch the parent chain, which
//     has no CT. Any of those variants on a confidential offer is rejected.
//   - MUST publish a maker blinding pubkey so the taker can blind the maker's
//     output, and a blinded (blech32 tsqb/sqb HRP) receive address, so BOTH legs
//     blind on-chain.
//   - MUST NOT involve the BTC sentinel on either pair side (parent chain, no CT).
//
// This makes a confidential offer settle fully blinded by construction: without a
// blinded maker output there is no second blinded leg, and the public swap ratio
// would leak the amount.
func (v *Validator) checkConfidential(o *seqobv1.Offer) error {
	if o.GetCrossChain() != nil || o.GetLightning() != nil || offer.IsCovenant(o) {
		return fmt.Errorf("confidential offer must use the same-chain co-sign rail (no covenant/cross-chain/lightning)")
	}
	sc := o.GetSameChain()
	if sc == nil {
		return fmt.Errorf("confidential offer must carry same_chain terms")
	}
	p := o.GetPair()
	if offer.IsBTCSentinel(p.GetBaseAsset()) || offer.IsBTCSentinel(p.GetQuoteAsset()) ||
		offer.IsBTCSentinel(o.GetOfferAsset()) || offer.IsBTCSentinel(o.GetWantAsset()) {
		return fmt.Errorf("confidential book excludes BTC pairs (the parent chain has no confidential transactions)")
	}
	bp := sc.GetMakerBlindingPub()
	if bp == "" {
		return fmt.Errorf("confidential offer must publish maker_blinding_pub")
	}
	b, err := hex.DecodeString(bp)
	if err != nil {
		return fmt.Errorf("maker_blinding_pub not hex: %v", err)
	}
	if _, err := btcec.ParsePubKey(b); err != nil {
		return fmt.Errorf("maker_blinding_pub invalid: %v", err)
	}
	addr := sc.GetMakerRecvAddress()
	if !isBlindedAddress(addr) {
		return fmt.Errorf("confidential offer maker_recv_address must be a blinded (blech32) address")
	}
	return nil
}

// isBlindedAddress reports whether addr is a Sequentia confidential (blinded)
// address: the opt-in blech32 form uses the tsqb (testnet) / sqb (mainnet) HRP,
// distinct from the transparent bech32 tb1/bc1 form (Principle 6). A confidential
// offer must receive to a blinded address so the maker's leg blinds on-chain.
func isBlindedAddress(addr string) bool {
	a := strings.ToLower(strings.TrimSpace(addr))
	return strings.HasPrefix(a, "tsqb1") || strings.HasPrefix(a, "sqb1")
}

// checkLightning validates a submarine-swap (asset on-chain <-> BTC-over-Lightning)
// offer's LightningTerms. Like the cross-chain case the resting keys are advisory
// (the load-bearing SEQ-claim key is minted per-lift and bound by recomputing the
// redeemScript at settlement); here we only check well-formedness. Convention:
// base = the SEQ asset, quote = the BTC sentinel.
func (v *Validator) checkLightning(o *seqobv1.Offer) error {
	lt := o.GetLightning()
	if lt == nil {
		return fmt.Errorf("lightning offer missing lightning terms")
	}
	p := o.GetPair()
	baseBTC, quoteBTC := offer.IsBTCSentinel(p.GetBaseAsset()), offer.IsBTCSentinel(p.GetQuoteAsset())
	// EVERY Lightning direction is asset-agnostic on the quote side now:
	//   - Pure-LN (2/3): two Lightning HTLCs bound by one preimage; quote = BTC
	//     or a real asset (asset<->asset pure-LN markets, e.g. GOLD/EURX).
	//   - Submarine (0/1) and sub-asset (4/5): one Lightning leg + one ON-CHAIN
	//     HTLC. Quote = the BTC sentinel (the on-chain leg is real parent-chain
	//     BTC) or a REAL asset id — the MIXED SAME-CHAIN shape (rails 7/8),
	//     where the on-chain leg is a Sequentia HTLC on the quote asset,
	//     standing exactly where BTC stands (no structurally privileged unit).
	// The base is always a real Sequentia asset, and a pair never repeats an
	// asset on both sides.
	if baseBTC {
		return fmt.Errorf("lightning pair base must be an asset (quote is %s or a real asset id)", offer.BTCSentinel)
	}
	if !quoteBTC {
		if b, err := hex.DecodeString(p.GetQuoteAsset()); err != nil || len(b) != 32 {
			return fmt.Errorf("lightning pair quote must be %s or a 64-hex asset id", offer.BTCSentinel)
		}
		if p.GetBaseAsset() == p.GetQuoteAsset() {
			return fmt.Errorf("lightning pair cannot be the same asset on both sides")
		}
	}
	// Advisory HTLC keys are raw compressed pubkey bytes (LightningTerms uses bytes,
	// unlike CrossChainTerms' hex strings).
	for _, pk := range []struct {
		name string
		b    []byte
	}{
		{"maker_claim_pub", lt.GetMakerClaimPub()},
		{"maker_refund_pub", lt.GetMakerRefundPub()},
	} {
		if _, err := btcec.ParsePubKey(pk.b); err != nil {
			return fmt.Errorf("lightning %s invalid: %v", pk.name, err)
		}
	}
	if lt.GetLnDirection() > 5 {
		return fmt.Errorf("lightning ln_direction must be 0/1 (submarine: asset<->BTC-LN), 2/3 (pure-LN: asset-LN<->BTC-LN), or 4/5 (sub-asset: asset-LN<->BTC-on-chain)")
	}
	if !offer.LnDirectionConsistent(lt.GetLnDirection(), o.GetTradeDir() == seqobv1.TradeDir_TRADE_DIR_SELL) {
		return fmt.Errorf("lightning ln_direction inconsistent with trade_dir")
	}
	// The maker's LN node id is required when the TAKER must pay the maker: the
	// reverse submarine direction, and BOTH pure-LN directions (the taker pays the
	// maker's hold by bare hash, so it needs the maker's hold-leg node id). For the
	// normal submarine direction the maker pays the taker's invoice, so it is
	// optional there. Validate it whenever present.
	if (lt.GetLnDirection() == offer.LnBTCForAsset || offer.IsPureLN(lt.GetLnDirection())) && o.GetMakerLnNodePubkey() == "" {
		return fmt.Errorf("lightning reverse/pure-LN offer must advertise maker_ln_node_pubkey")
	}
	if npk := o.GetMakerLnNodePubkey(); npk != "" {
		b, err := hex.DecodeString(npk)
		if err != nil {
			return fmt.Errorf("maker_ln_node_pubkey not hex: %v", err)
		}
		if _, err := btcec.ParsePubKey(b); err != nil {
			return fmt.Errorf("maker_ln_node_pubkey invalid: %v", err)
		}
	}
	return nil
}

// checkCrossChain validates a cross-chain (BTC<->asset) offer's CrossChainTerms.
// The resting offer is DISCOVERY-ONLY: maker_claim_pub/maker_refund_pub/
// maker_leg_locktime are advisory (display + a stable signed commitment); the
// load-bearing HTLC keys and locktimes are minted per-lift over the E2E courier and
// bound at settlement by recomputing the redeemScript byte-for-byte. Here we only
// check the offer is well-formed and self-consistent. Convention: base = the SEQ
// asset, quote = the BTC sentinel.
func (v *Validator) checkCrossChain(o *seqobv1.Offer) error {
	cc := o.GetCrossChain()
	if cc == nil {
		return fmt.Errorf("cross-chain offer missing cross_chain terms")
	}
	p := o.GetPair()
	baseBTC, quoteBTC := offer.IsBTCSentinel(p.GetBaseAsset()), offer.IsBTCSentinel(p.GetQuoteAsset())
	if baseBTC == quoteBTC {
		return fmt.Errorf("cross-chain pair must have exactly one BTC-sentinel side")
	}
	if !quoteBTC {
		return fmt.Errorf("cross-chain pair must be base=asset, quote=%s", offer.BTCSentinel)
	}
	if cc.GetBtcSentinel() != offer.BTCSentinel {
		return fmt.Errorf("cross_chain btc_sentinel must be %q", offer.BTCSentinel)
	}
	for _, pk := range []struct{ name, hexv string }{
		{"maker_claim_pub", cc.GetMakerClaimPub()},
		{"maker_refund_pub", cc.GetMakerRefundPub()},
	} {
		b, err := hex.DecodeString(pk.hexv)
		if err != nil {
			return fmt.Errorf("cross_chain %s not hex: %v", pk.name, err)
		}
		if _, err := btcec.ParsePubKey(b); err != nil {
			return fmt.Errorf("cross_chain %s invalid: %v", pk.name, err)
		}
	}
	if cc.GetMakerLegLocktime() == 0 {
		return fmt.Errorf("cross_chain maker_leg_locktime must be > 0")
	}
	if cc.GetDirection() > 1 {
		return fmt.Errorf("cross_chain direction must be 0 (BTC->asset) or 1 (asset->BTC)")
	}
	if !offer.DirectionConsistent(cc.GetDirection(), o.GetTradeDir() == seqobv1.TradeDir_TRADE_DIR_SELL) {
		return fmt.Errorf("cross_chain direction inconsistent with trade_dir")
	}
	return nil
}

func (v *Validator) checkExpiry(o *seqobv1.Offer) error {
	now := v.cfg.Now()
	exp := o.GetExpiresAtUnix()
	if exp == 0 {
		return fmt.Errorf("expires_at_unix is mandatory")
	}
	t := time.Unix(int64(exp), 0)
	if !t.After(now) {
		return fmt.Errorf("offer already expired")
	}
	if v.cfg.MinExpiry > 0 && t.Before(now.Add(v.cfg.MinExpiry)) {
		return fmt.Errorf("expiry too soon (min %s)", v.cfg.MinExpiry)
	}
	if v.cfg.MaxExpiry > 0 && t.After(now.Add(v.cfg.MaxExpiry)) {
		return fmt.Errorf("expiry too far in the future (max %s)", v.cfg.MaxExpiry)
	}
	return nil
}

// checkIPRate enforces the per-IP sliding-window limit. It is keyed on the
// connection (not the maker_pubkey) and is charged before signature verification
// so a flood is throttled cheaply. ip may be empty (limit skipped).
func (v *Validator) checkIPRate(ip string) error {
	if ip == "" || v.cfg.MaxOffersPerMinPerIP <= 0 {
		return nil
	}
	now := v.cfg.Now()
	cutoff := now.Add(-time.Minute)
	v.mu.Lock()
	defer v.mu.Unlock()
	v.sweepLocked(now, cutoff)
	hits := prune(v.ipHits[ip], cutoff)
	if len(hits) >= v.cfg.MaxOffersPerMinPerIP {
		v.ipHits[ip] = hits
		return fmt.Errorf("rate limit: too many offers from this IP")
	}
	v.ipHits[ip] = append(hits, now)
	return nil
}

// checkPubkeyRate enforces the per-maker_pubkey sliding-window limit. The CALLER
// MUST have already verified maker_sig: maker_pubkey is attacker-replayable
// public data, so only a genuine maker (which alone can produce a valid sig) may
// reach here and consume its own budget.
func (v *Validator) checkPubkeyRate(makerPubkey string) error {
	if v.cfg.MaxOffersPerMinPerPubkey <= 0 {
		return nil
	}
	now := v.cfg.Now()
	cutoff := now.Add(-time.Minute)
	v.mu.Lock()
	defer v.mu.Unlock()
	v.sweepLocked(now, cutoff)
	hits := prune(v.pubHits[makerPubkey], cutoff)
	if len(hits) >= v.cfg.MaxOffersPerMinPerPubkey {
		v.pubHits[makerPubkey] = hits
		return fmt.Errorf("rate limit: too many offers for this maker_pubkey")
	}
	v.pubHits[makerPubkey] = append(hits, now)
	return nil
}

// ValidateCancel verifies a signed cancel's signature. (Nonce/replay enforcement
// is the offerstore's job, since it holds the per-key nonce high-water mark.)
func (v *Validator) ValidateCancel(c *seqobv1.OfferCancel) error {
	return offer.VerifyCancel(c)
}

// sweepLocked drops every rate-limit key whose window has emptied, at most once a
// minute. The caller holds v.mu.
func (v *Validator) sweepLocked(now, cutoff time.Time) {
	if now.Sub(v.lastSweep) < time.Minute {
		return
	}
	v.lastSweep = now
	for k, hits := range v.ipHits {
		if len(prune(hits, cutoff)) == 0 {
			delete(v.ipHits, k)
		}
	}
	for k, hits := range v.pubHits {
		if len(prune(hits, cutoff)) == 0 {
			delete(v.pubHits, k)
		}
	}
}

func prune(hits []time.Time, cutoff time.Time) []time.Time {
	out := hits[:0]
	for _, h := range hits {
		if h.After(cutoff) {
			out = append(out, h)
		}
	}
	return out
}
