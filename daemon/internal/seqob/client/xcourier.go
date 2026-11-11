package client

import (
	"encoding/json"
	"fmt"
)

// xcourier.go defines the CROSS-CHAIN lift handshake messages. They travel as the
// plaintext sealed inside the existing opaque relay courier (SwapMsg.ciphertext):
// the relay only ever moves sealed bytes and never parses these. They are JSON
// (not protobuf) by deliberate choice: both endpoints are new code, the relay is
// blind, and JSON keeps the multi-round HTLC handshake easy to evolve. The on-chain
// settlement itself is the proven pkg/xchain HTLC swap; this is only the negotiation
// + leg-exchange transport that replaces the RFQ HTTP round-trips.
//
// Handshake (taker always initiates the courier exchange, as in the same-chain path):
//
//	FORWARD  (offer.direction = BTC_TO_ASSET; taker pays BTC, secret holder = TAKER)
//	  taker -> XcTermsRequest
//	  maker -> XcTerms{maker_btc_claim_pub, maker_refund_pub (seq), btc/seq locktime+amount, fee_btc}
//	  taker -> XcBtcLegFunded{hash_h, leg(BTC), taker_seq_claim_pub, taker_btc_refund_pub}
//	  maker -> XcSeqLegLocked{leg(SEQ, with block_hash+anchor_height)}
//	  taker  : VerifySeqLegSafe (anchor gate) then ClaimSEQLeg -> reveals s on-chain
//	  maker  : watches the SEQ claim, extracts s, ClaimBTCLeg
//
//	REVERSE  (offer.direction = ASSET_TO_BTC; taker sells the asset, secret holder = MAKER)
//	  taker -> XcTermsRequest{taker_seq_refund_pub, taker_btc_claim_pub}
//	           (the BTC HTLC's claim branch pays the taker, so its key must reach
//	           the maker BEFORE the maker funds that leg)
//	  maker -> XcBtcLegLocked{hash_h, leg(BTC), maker_seq_claim_pub, maker_refund_pub (btc),
//	           seq locktime, btc/seq amounts (the terms ride in this message)}
//	  taker  : verify the BTC leg, await its confirmation, fund the SEQ leg
//	  taker -> XcSeqLegFunded{leg(SEQ, with block_hash+anchor_height)}
//	  maker  : VerifySeqLegSafe then ClaimSEQLeg -> reveals s
//	  maker -> XcSecretRevealed{preimage}   (taker can also read s off the SEQ-claim scriptSig)
//	  taker  : ClaimBTCLeg
type XcMsgType string

const (
	XcTermsRequest   XcMsgType = "terms_request"
	XcTerms          XcMsgType = "terms"           // forward: maker's per-lift terms
	XcBtcLegFunded   XcMsgType = "btc_leg_funded"  // forward: taker funded the BTC leg
	XcSeqLegLocked   XcMsgType = "seq_leg_locked"  // forward: maker locked the SEQ leg
	XcBtcLegLocked   XcMsgType = "btc_leg_locked"  // reverse: maker locked the BTC leg
	XcSeqLegFunded   XcMsgType = "seq_leg_funded"  // reverse: taker funded the SEQ leg
	XcSecretRevealed XcMsgType = "secret_revealed" // reverse: maker reveals s after claiming SEQ
	XcFail           XcMsgType = "fail"
)

// XcLeg is a funded HTLC leg conveyed over the courier. Byte fields are hex. The
// receiver re-derives and byte-compares the redeem script before trusting any leg
// (the relay and the message are untrusted); amounts/asset/locktime bind it to terms.
type XcLeg struct {
	Txid         string `json:"txid"`
	Vout         uint32 `json:"vout"`
	Amount       uint64 `json:"amount"`
	Asset        string `json:"asset,omitempty"` // SEQ leg only (asset id hex)
	RedeemScript string `json:"redeem_script"`   // hex
	Locktime     uint32 `json:"locktime"`
	Height       int64  `json:"height,omitempty"`        // confirmed height (BTC leg, for anchor ordering)
	BlockHash    string `json:"block_hash,omitempty"`    // SEQ leg, for the anchor gate
	AnchorHeight int64  `json:"anchor_height,omitempty"` // SEQ leg's Bitcoin-anchor height
}

// XcMsg is the tagged union for the cross-chain courier. Only the fields relevant to
// Type are set; omitempty keeps each message compact.
type XcMsg struct {
	Type XcMsgType `json:"type"`

	// Terms (maker -> taker, forward) / BtcLegLocked terms (reverse).
	MakerBtcClaimPub string `json:"maker_btc_claim_pub,omitempty"` // forward: maker claims BTC with s
	MakerSeqClaimPub string `json:"maker_seq_claim_pub,omitempty"` // reverse: maker claims SEQ with s
	MakerRefundPub   string `json:"maker_refund_pub,omitempty"`    // forward: maker's SEQ refund; reverse: maker's BTC refund
	BtcLocktime      uint32 `json:"btc_locktime,omitempty"`        // T_btc (the longer leg)
	SeqLocktime      uint32 `json:"seq_locktime,omitempty"`        // T_seq (the shorter leg)
	BtcAmount        uint64 `json:"btc_amount,omitempty"`          // sats
	SeqAmount        uint64 `json:"seq_amount,omitempty"`          // asset atoms
	FeeBtc           uint64 `json:"fee_btc,omitempty"`

	// Secret hash + taker pubkeys.
	HashH             string `json:"hash_h,omitempty"`               // sha256(secret), hex
	TakerSeqClaimPub  string `json:"taker_seq_claim_pub,omitempty"`  // forward: taker claims SEQ with s
	TakerBtcRefundPub string `json:"taker_btc_refund_pub,omitempty"` // forward: taker refunds BTC after T_btc
	TakerSeqRefundPub string `json:"taker_seq_refund_pub,omitempty"` // reverse: taker refunds SEQ after T_seq
	TakerBtcClaimPub  string `json:"taker_btc_claim_pub,omitempty"`  // reverse: taker claims BTC with s

	Leg *XcLeg `json:"leg,omitempty"` // the leg this message conveys

	// Submarine-swap (Lightning) fields. The BTC leg is a BOLT11 payment, not an
	// on-chain HTLC, so these carry it instead of an XcLeg. See xcourier_submarine.go.
	Bolt11         string `json:"bolt11,omitempty"`           // submarine: the invoice bound to H; pure-LN: the taker's asset invoice
	MinAnchorDepth uint32 `json:"min_anchor_depth,omitempty"` // submarine: the gate depth (transparency)
	SettleTxid     string `json:"settle_txid,omitempty"`      // submarine: the on-chain asset-claim txid (informational)

	// Pure-LN (both legs Lightning) fields. See xcourier_pureln.go.
	MakerLNNodeID string `json:"maker_ln_node_id,omitempty"` // pure-LN: the maker's incoming-leg node id (the taker pays its hold by bare hash)
	HoldCltv      uint32 `json:"hold_cltv,omitempty"`        // pure-LN hold_ready: final-hop timelock (hold-leg blocks) the taker must pay the hold with
	TakerLNNodeID string `json:"taker_ln_node_id,omitempty"` // sub-asset BUY HODL: the taker's HOSTED asset node id; the MAKER pays the hold by BARE HASH to it (device holds P, no bolt11)

	// Restricted-asset swap fields (RestrictedTerms with a BTC quote). The
	// Sequentia leg is the policy server's swap HTLC with its fee pot; see
	// xdriver_restricted.go.
	TakerAID  string  `json:"taker_aid,omitempty"`  // terms_request: the taker's account at the asset's policy server
	PotAsset  string  `json:"pot_asset,omitempty"`  // terms / btc_leg_locked: the pot's asset (display hex)
	PotAmount uint64  `json:"pot_amount,omitempty"` // terms / btc_leg_locked: the pot's atoms
	PotVout   *uint32 `json:"pot_vout,omitempty"`   // seq_leg_locked / seq_leg_funded: the pot's output in the lock
	Payout    string  `json:"payout,omitempty"`     // seq_leg_locked / seq_leg_funded: the seller's v1 script the pot goes home to (hex)

	Preimage string `json:"preimage,omitempty"` // XcSecretRevealed
	Code     string `json:"code,omitempty"`     // XcFail
	Message  string `json:"message,omitempty"`  // XcFail
}

// Seal JSON-marshals the message and seals it for the courier with the session Crypter.
func (m *XcMsg) Seal(c *Crypter) ([]byte, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal xc msg: %w", err)
	}
	return c.Seal(b)
}

// OpenXcMsg opens a sealed cross-chain courier message with the session Crypter.
func OpenXcMsg(sealed []byte, c *Crypter) (*XcMsg, error) {
	pt, err := c.Open(sealed)
	if err != nil {
		return nil, fmt.Errorf("open xc msg: %w", err)
	}
	var m XcMsg
	if err := json.Unmarshal(pt, &m); err != nil {
		return nil, fmt.Errorf("unmarshal xc msg: %w", err)
	}
	return &m, nil
}

// failMsg builds an XcFail to courier on a handshake error.
func failMsg(code, msg string) *XcMsg { return &XcMsg{Type: XcFail, Code: code, Message: msg} }
