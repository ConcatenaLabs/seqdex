package offer

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
)

// TradeReport is a maker's signed record of one trade it settled on a rail the
// relay cannot see for itself (an in-tree swap of its leaves, its BTC desk): it
// gave Give atoms of GiveAsset for Want atoms of WantAsset, in the fill named ID
// (the maker's swap id, unique to the fill). The relay records it only from a
// reporter it trusts (seqobd -trade-reporters), once per (reporter, rail, ID).
type TradeReport struct {
	Reporter  string `json:"reporter"` // compressed secp256k1 public key, hex
	Rail      string `json:"rail"`
	ID        string `json:"id"`
	GiveAsset string `json:"give_asset"` // asset id (display hex) or "BTC"
	Give      string `json:"give"`       // atoms, decimal
	WantAsset string `json:"want_asset"`
	Want      string `json:"want"`
	Sig       []byte `json:"sig"` // DER, low-S, over TradeReportHash (base64 in JSON)
}

// TradeReportHash is what a reporter signs: every field of the report but the
// signature, under a tag of its own, so the signature authorises this one
// record and can be read as no other message the relay accepts.
func TradeReportHash(r *TradeReport) [32]byte {
	return sha256.Sum256([]byte(strings.Join([]string{"seqob-trade-report-v1",
		strings.ToLower(r.Reporter), r.Rail, r.ID, r.GiveAsset, r.Give, r.WantAsset, r.Want}, "|")))
}

var (
	reportAsset = regexp.MustCompile(`^([0-9a-f]{64}|BTC)$`)
	reportWord  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	reportID    = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
)

// ParseTradeReport checks a report's form and signature and returns its two
// amounts.
func ParseTradeReport(r *TradeReport) (give, want uint64, err error) {
	if r == nil {
		return 0, 0, errors.New("nil trade report")
	}
	if !reportWord.MatchString(r.Rail) {
		return 0, 0, fmt.Errorf("bad rail %q", r.Rail)
	}
	if !reportID.MatchString(r.ID) {
		return 0, 0, errors.New("bad id")
	}
	if !reportAsset.MatchString(r.GiveAsset) || !reportAsset.MatchString(r.WantAsset) || r.GiveAsset == r.WantAsset {
		return 0, 0, errors.New("a report names two assets, each an asset id or BTC")
	}
	if give, err = strconv.ParseUint(r.Give, 10, 64); err != nil || give == 0 {
		return 0, 0, errors.New("bad give")
	}
	if want, err = strconv.ParseUint(r.Want, 10, 64); err != nil || want == 0 {
		return 0, 0, errors.New("bad want")
	}
	pub, err := parsePubkeyHex(r.Reporter)
	if err != nil {
		return 0, 0, err
	}
	sig, err := parseCanonicalSig(r.Sig, "trade report sig")
	if err != nil {
		return 0, 0, err
	}
	h := TradeReportHash(r)
	if !sig.Verify(h[:], pub) {
		return 0, 0, errors.New("trade report sig verification failed")
	}
	return give, want, nil
}

// SignTradeReport fills r.Reporter (from priv) and r.Sig.
func SignTradeReport(r *TradeReport, priv *btcec.PrivateKey) {
	r.Reporter = fmt.Sprintf("%x", priv.PubKey().SerializeCompressed())
	h := TradeReportHash(r)
	r.Sig = ecdsa.Sign(priv, h[:]).Serialize()
}
