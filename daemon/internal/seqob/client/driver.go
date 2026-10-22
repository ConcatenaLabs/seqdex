package client

import (
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	seqdexv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqdex/v1"
	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
)

// Taker drives the proposer side of a lift. It builds the SwapRequest, seals it
// for the relay courier, and finalizes the SwapAccept it gets back. The relay
// only ever moves the sealed bytes (SwapMsg.ciphertext), never the plaintext.
type Taker struct {
	Wallet Wallet
}

// Propose builds and seals the SwapRequest for an offer lift. It returns both the
// sealed bytes to courier and the plaintext SwapRequest (for the caller's logs).
func (t *Taker) Propose(o *seqobv1.Offer, takeBase uint64, feeAsset string, c *Crypter) (sealed []byte, req *seqdexv1.SwapRequest, err error) {
	req, err = t.Wallet.ProposerBuildRequest(o, takeBase, feeAsset)
	if err != nil {
		return nil, nil, fmt.Errorf("build request: %w", err)
	}
	pt, err := proto.Marshal(req)
	if err != nil {
		return nil, nil, err
	}
	sealed, err = c.Seal(pt)
	if err != nil {
		return nil, nil, err
	}
	return sealed, req, nil
}

// Finalize opens the maker's sealed SwapAccept, blinds+signs+broadcasts via the
// wallet, and returns the sealed SwapComplete plus the broadcast txid.
func (t *Taker) Finalize(sealedAccept []byte, c *Crypter) (sealedComplete []byte, txid string, err error) {
	pt, err := c.Open(sealedAccept)
	if err != nil {
		return nil, "", fmt.Errorf("open accept: %w", err)
	}
	var acc seqdexv1.SwapAccept
	uerr := proto.Unmarshal(pt, &acc)
	if uerr != nil || acc.GetTransaction() == "" {
		// The maker refused the co-sign (a sealed SwapFail): say why, at once.
		var fail seqdexv1.SwapFail
		if proto.Unmarshal(pt, &fail) == nil && fail.GetFailureMessage() != "" {
			return nil, "", fmt.Errorf("%w: %s", ErrMakerRefused, fail.GetFailureMessage())
		}
	}
	if uerr != nil {
		return nil, "", fmt.Errorf("unmarshal accept: %w", uerr)
	}
	complete, txid, err := t.Wallet.ProposerFinalize(&acc)
	if err != nil {
		return nil, "", fmt.Errorf("finalize: %w", err)
	}
	ct, err := proto.Marshal(complete)
	if err != nil {
		return nil, "", err
	}
	sealedComplete, err = c.Seal(ct)
	if err != nil {
		return nil, "", err
	}
	return sealedComplete, txid, nil
}

// Maker drives the responder side: it opens the taker's sealed SwapRequest, runs
// the existing CompleteSwap path, and seals the SwapAccept back.
type Maker struct {
	Wallet Wallet
	// Offer is the maker's OWN signed resting offer. When set, HandleRequest binds
	// every co-sign to it (asset legs, price floor, remaining size) so a malicious
	// taker cannot drain the maker at an arbitrary price. Left nil only in unit
	// tests that exercise the raw message flow.
	Offer *seqobv1.Offer
}

// HandleRequest opens a sealed SwapRequest, runs ResponderComplete, and returns
// the sealed SwapAccept.
//
// ITEM C (relay-MITM re-attack: VERIFIED FALSE POSITIVE; DoS only, NOT a CT leak).
// The Crypter c may have been derived from a relay-supplied taker session pubkey
// (see NewMakerCrypterFromLift). That is safe by ORDERING: this method's FIRST act
// is c.Open(sealedReq), and only on success does it go on to Seal a SwapAccept.
// The taker sealed sealedReq under the key from the real maker+taker pubkeys, so a
// relay that substituted its own pubkey yields a mismatched key, c.Open below
// fails, and the method returns before any SwapAccept exists, leaving nothing the
// relay can decrypt. Key substitution is therefore a denial-of-service (the lift
// fails), never a confidentiality leak.
func (m *Maker) HandleRequest(sealedReq []byte, c *Crypter) (sealedAccept []byte, err error) {
	// Open-before-Seal (see the ITEM C note above): a wrong/substituted key makes
	// this fail, so no SwapAccept is ever produced or leaked.
	pt, err := c.Open(sealedReq)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOpenRequest, err)
	}
	var req seqdexv1.SwapRequest
	if err := proto.Unmarshal(pt, &req); err != nil {
		return nil, fmt.Errorf("unmarshal request: %w", err)
	}
	// SECURITY (maker drain): bind the co-sign to the maker's own offer before
	// touching the wallet, so the decrypted request cannot move funds at a price
	// or in assets the maker never offered.
	if m.Offer != nil {
		if err := ValidateRequestAgainstOffer(&req, m.Offer); err != nil {
			return nil, fmt.Errorf("request does not match offer: %w", err)
		}
	}
	acc, err := m.Wallet.ResponderComplete(&req)
	if err != nil {
		return nil, fmt.Errorf("responder complete: %w", err)
	}
	at, err := proto.Marshal(acc)
	if err != nil {
		return nil, err
	}
	sealedAccept, err = c.Seal(at)
	if err != nil {
		return nil, err
	}
	return sealedAccept, nil
}

// ErrOpenRequest: the taker's sealed request did not open under this session's key (a
// substituted key, or garbage). Nothing is answered then: a refusal sealed under that key
// would be readable by whoever substituted it.
var ErrOpenRequest = errors.New("open request")

// ErrMakerRefused: the maker answered the lift with a SwapFail; nothing was signed.
var ErrMakerRefused = errors.New("maker refused the co-sign")

// Refusal codes a maker answers in a sealed SwapFail, so a taker learns at once that the
// co-sign is refused instead of waiting out its deadline.
const (
	FailCannotFund   uint32 = 1 // the maker cannot fund this lift now (no spendable coins)
	FailNotTheOffer  uint32 = 2 // the request does not match the offer lifted
	FailCannotCoSign uint32 = 3 // any other reason the maker could not co-sign
)

// SealSwapFail seals a seqdex.v1.SwapFail for the taker of this session. The message says
// what kind of refusal it is in plain words; it names no coin and no wallet detail.
func SealSwapFail(c *Crypter, id string, code uint32) ([]byte, error) {
	msg := map[uint32]string{
		FailCannotFund:   "the maker cannot fund this lift right now",
		FailNotTheOffer:  "the request does not match the offer",
		FailCannotCoSign: "the maker could not co-sign this lift",
	}[code]
	if msg == "" {
		code, msg = FailCannotCoSign, "the maker could not co-sign this lift"
	}
	b, err := proto.Marshal(&seqdexv1.SwapFail{Id: id, FailureCode: code, FailureMessage: msg})
	if err != nil {
		return nil, err
	}
	return c.Seal(b)
}

// HandleComplete opens the taker's sealed SwapComplete and returns the settling txid, so the
// maker can ack settlement to the relay. The relay never parses the opaque courier, so this
// ack is its only signal that an interactive lift settled (without it the resting order lingers
// as a ghost and the trades feed stays empty). Best-effort on the txid: a parse failure returns
// "" and the caller should still ack — the relay's trade record + decrement do not need it.
func (m *Maker) HandleComplete(sealedComplete []byte, c *Crypter) (txid string, err error) {
	pt, err := c.Open(sealedComplete)
	if err != nil {
		return "", fmt.Errorf("open complete: %w", err)
	}
	var cmp seqdexv1.SwapComplete
	if err := proto.Unmarshal(pt, &cmp); err != nil {
		return "", fmt.Errorf("unmarshal complete: %w", err)
	}
	return txidFromSignedPset(cmp.GetTransaction()), nil
}
