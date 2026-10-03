package xchain

import (
	"errors"
	"fmt"
	"strings"
)

// PreimageLen is the only preimage length Lightning settles.
const PreimageLen = 32

var (
	// errNoSecret is returned when a redeem spend is requested but the
	// HashLock holds only the hash, not the preimage.
	errNoSecret = errors.New("xchain: hashlock has no secret/preimage to build a redeem spend")

	// ErrAnchorOrdering is returned by the orchestrator when the Sequentia leg
	// does NOT satisfy the anchor-shortened ordering rule (its block's
	// anchorheight is below the BTC-leg's confirmation height, or the anchor
	// status is not "ok"). Proceeding would forfeit the reorg-safety that
	// anchoring provides, so the orchestrator refuses.
	ErrAnchorOrdering = errors.New("xchain: SEQ leg violates anchor-shortened ordering (anchorheight < BTC-leg height, or anchor not ok)")

	// ErrAnchorOrderingTerminal is the subset of ErrAnchorOrdering that can NEVER
	// clear by waiting: the SEQ leg's OWN confirming block commits an anchorheight
	// BELOW the BTC-leg height. anchorheight is a committed CBlockHeader field
	// (primitives/block.h m_anchor_height), so for a given block hash it is
	// immutable — re-reading it every poll is futile by construction.
	//
	// The VERDICT is unchanged (still a refusal, and it still wraps
	// ErrAnchorOrdering so every errors.Is(err, ErrAnchorOrdering) caller keeps
	// working). The sentinel exists only so a caller stops burning its timelock
	// window on a hopeless retry loop while the two conjuncts that DO vary
	// (anchorstatus flap, not-yet-certified) keep being retried.
	//
	// It must NOT be "cleared" by re-reading the anchor of a LATER (burying) block
	// or of the chain tip. InvalidateBlock disconnects the offending block and
	// everything ABOVE it while lower blocks stay CONNECTED, so a burying block's
	// anchor says nothing about whether the FUNDING output survives a Bitcoin
	// reorg — only the funding block's own anchor does.
	ErrAnchorOrderingTerminal = fmt.Errorf("%w [TERMINAL: the leg's own block anchors below the BTC leg; a committed header field that can never rise]", ErrAnchorOrdering)

	// ErrBTCLegInvalid is returned by the maker when the taker's claimed BTC leg
	// does not match the quote (wrong H, wrong redeemScript, wrong amount/asset,
	// or it does not pay the expected HTLC P2SH). The maker MUST refuse to lock
	// its SEQ leg in this case.
	ErrBTCLegInvalid = errors.New("xchain: taker BTC leg invalid (does not match quote)")

	// ErrBTCLegUnconfirmed is returned when the taker's BTC leg is not yet
	// confirmed to the required depth. The taker must confirm it before the
	// maker will lock the SEQ leg (the BTC-leg-first ordering rule).
	ErrBTCLegUnconfirmed = errors.New("xchain: taker BTC leg not confirmed to required depth")

	// ErrBTCLegNotSeen means the BTC-leg funding tx cannot be found on THIS node
	// at all yet (getrawtransaction -5 "No such mempool or blockchain
	// transaction"). A taker funds and announces within seconds, so a fresh
	// 0-conf leg can legitimately reach us before its tx has propagated to our
	// own node — that is NOT evidence the leg is invalid, only that we have not
	// seen it. It is deliberately DISTINCT from ErrBTCLegInvalid: Swap.VerifyBTCLeg
	// polls this class out (bounded) before declaring the leg invalid, and the
	// courier drivers' verify loops treat it as retryable, not terminal.
	ErrBTCLegNotSeen = errors.New("xchain: BTC leg funding tx not seen on this node yet")

	// ErrSEQLegInvalid is the reverse (asset->BTC) mirror of ErrBTCLegInvalid:
	// returned when the taker's funded SEQ asset leg does not match the quote
	// (wrong H, wrong redeemScript, wrong amount/asset, or it does not pay the
	// expected HTLC P2SH). The maker MUST refuse to reveal the secret (claim the
	// SEQ leg) in this case.
	ErrSEQLegInvalid = errors.New("xchain: taker SEQ leg invalid (does not match quote)")

	// ErrSEQLegUnconfirmed is the reverse mirror of ErrBTCLegUnconfirmed: the
	// taker's SEQ asset leg is not yet confirmed to the required depth.
	ErrSEQLegUnconfirmed = errors.New("xchain: taker SEQ leg not confirmed to required depth")

	// ErrLNLegInvalid means the Lightning leg of a submarine swap does not match
	// the swap (invoice hash != H, wrong amount, or a revealed preimage that does
	// not hash to H). The maker MUST NOT proceed.
	ErrLNLegInvalid = errors.New("xchain: submarine LN leg invalid (does not match swap)")

	// ErrLNLegTimeout means a Lightning-leg wait (e.g. a hold invoice never
	// reaching "accepted") exceeded its deadline; fall back to the refund path.
	ErrLNLegTimeout = errors.New("xchain: submarine LN leg timed out")

	// ErrBadPreimageLen means a preimage is not the 32 bytes Lightning settles. The
	// on-chain HTLC script accepts any length whose sha256 matches, so a value of
	// another length is a claim that could never resolve a Lightning hold.
	ErrBadPreimageLen = errors.New("xchain: preimage is not 32 bytes")

	// ErrLNPayUnresolved means an outgoing Lightning payment could be neither
	// confirmed complete nor confirmed failed: the node still reports it pending
	// after the bounded wait. The HTLC may settle later and the node would then
	// hold the preimage. A caller holding an INCOMING leg against it must not
	// cancel that hold on this error — that is the one outcome that loses money —
	// but treat the swap as in flight and resolve it from listpays.
	ErrLNPayUnresolved = errors.New("xchain: outgoing Lightning payment still pending (neither complete nor failed)")

	// ErrLNPayDeclined means the paying node's signer declined the payment before
	// any HTLC was offered. On a keyless (device-signed) node that is the device's
	// payment limit for the asset; nothing was sent, so the caller may cancel
	// whatever it holds against this payment.
	ErrLNPayDeclined = errors.New("xchain: the paying node's signer declined the payment (nothing was sent)")

	// ErrLNHoldAsset means a hold cannot be made or trusted in this leg's asset:
	// the leg names no asset on a network that has several, or the hold that was
	// accepted is in another asset than the leg's. The holder must not settle it,
	// so the preimage is never revealed for a payment in the wrong asset.
	ErrLNHoldAsset = errors.New("xchain: Lightning hold is not in this leg's asset")
)

// isTxNotFoundRPC reports whether err is the node's "transaction not found"
// class from a tx lookup (JSON-RPC code -5, e.g. getrawtransaction's "No such
// mempool or blockchain transaction"). ONLY this class is the propagation-lag
// retry case: the txid we asked about is well-formed (it came off a funded
// leg announcement), so -5 here can only mean the node has not seen the tx.
// Any other failure — including a tx that IS found but mismatches — is not.
func isTxNotFoundRPC(err error) bool {
	if err == nil {
		return false
	}
	var re *rpcErr
	if errors.As(err, &re) {
		return re.Code == -5
	}
	// A chain flattened upstream (a %v wrap) loses the *rpcErr but keeps the
	// canonical message text.
	return strings.Contains(err.Error(), "No such mempool")
}
