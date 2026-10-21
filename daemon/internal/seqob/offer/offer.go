// Package offer provides canonical serialization, signing, and verification of
// SeqOB order-book offers and cancels.
//
// An Offer is a signed price/size/expiry/keys intent (never a PSET, never named
// UTXOs). The authoritative exchange ratio is want_amount/offer_amount (both
// integers); there is deliberately no floating-point price in the signed bytes
// (review m1), so the canonical encoding is stable across the Go relay and any
// other client.
//
// Canonical bytes = deterministic protobuf encoding of every field EXCLUDING
// maker_sig, which by construction includes the whole `settlement` oneof, so
// every current and future settlement variant is authenticated. We sha256 those
// bytes and sign/verify with secp256k1 (btcec, the same library the rest of the
// repo uses: see pkg/xchain/keys.go and pkg/trade/wallet.go) against the 33-byte
// compressed maker_pubkey.
package offer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"google.golang.org/protobuf/proto"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
)

// deterministic marshals m with the deterministic option so the same logical
// message always produces the same bytes (stable field ordering, no map jitter).
func deterministic(m proto.Message) ([]byte, error) {
	return proto.MarshalOptions{Deterministic: true}.Marshal(m)
}

// CanonicalOfferBytes returns the deterministic encoding of o with maker_sig
// cleared. This is the exact byte string the maker signs and verifiers re-derive.
//
// For covenant offers (either member, CovenantTerms or SimplicityCovenantTerms)
// the resting outpoint (covenant_txid/covenant_vout) is also cleared: it is chain-derived state that the relay's watcher rewrites on
// every partial-fill re-rest (the remainder self-replicates at a new outpoint)
// while the maker is offline and cannot re-sign. The signature authenticates
// maker INTENT (assets, rate, program, keys, merkle path); funding is
// separately authenticated by trustlessly re-deriving the covenant spk from the
// signed terms plus a live gettxout on the claimed outpoint, so excluding the
// outpoint from the signed bytes cedes nothing.
func CanonicalOfferBytes(o *seqobv1.Offer) ([]byte, error) {
	if o == nil {
		return nil, errors.New("nil offer")
	}
	c := proto.Clone(o).(*seqobv1.Offer)
	c.MakerSig = nil
	if ct := c.GetCovenant(); ct != nil {
		ct.CovenantTxid = ""
		ct.CovenantVout = 0
	}
	if st := c.GetSimplicityCovenant(); st != nil {
		st.CovenantTxid = ""
		st.CovenantVout = 0
	}
	// A resting leaf order's locator (the coin it rests at, and what that coin
	// holds) moves at every fill while its maker is away: it is outside the
	// signed bytes for the same reason (resting.go).
	if rt := c.GetArcaResting(); rt != nil {
		rt.Coin = ""
		rt.Value = 0
	}
	return deterministic(c)
}

// OfferHash is sha256 over the canonical offer bytes.
func OfferHash(o *seqobv1.Offer) ([32]byte, error) {
	b, err := CanonicalOfferBytes(o)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(b), nil
}

// SignOffer fills o.MakerSig with a DER secp256k1 signature over OfferHash(o)
// using priv. If o.MakerPubkey is empty it is set from priv; if it is already
// set it must match priv's compressed public key.
func SignOffer(o *seqobv1.Offer, priv *btcec.PrivateKey) error {
	if o == nil {
		return errors.New("nil offer")
	}
	pubHex := hex.EncodeToString(priv.PubKey().SerializeCompressed())
	if o.MakerPubkey == "" {
		o.MakerPubkey = pubHex
	} else if !strings.EqualFold(o.MakerPubkey, pubHex) {
		return fmt.Errorf("maker_pubkey %q does not match signing key %q", o.MakerPubkey, pubHex)
	}
	h, err := OfferHash(o)
	if err != nil {
		return err
	}
	o.MakerSig = ecdsa.Sign(priv, h[:]).Serialize()
	return nil
}

// VerifyOffer checks o.MakerSig against o.MakerPubkey over OfferHash(o).
func VerifyOffer(o *seqobv1.Offer) error {
	if o == nil {
		return errors.New("nil offer")
	}
	if len(o.MakerSig) == 0 {
		return errors.New("missing maker_sig")
	}
	pub, err := parsePubkeyHex(o.MakerPubkey)
	if err != nil {
		return err
	}
	sig, err := parseCanonicalSig(o.MakerSig, "maker_sig")
	if err != nil {
		return err
	}
	h, err := OfferHash(o)
	if err != nil {
		return err
	}
	if !sig.Verify(h[:], pub) {
		return errors.New("maker_sig verification failed")
	}
	return nil
}

// CanonicalCancelBytes returns the deterministic encoding of c with sig cleared.
// The signed payload is {offer_id, maker_pubkey, nonce}; the nonce defeats replay
// of an old cancel against a re-posted same offer_id (review m2).
func CanonicalCancelBytes(c *seqobv1.OfferCancel) ([]byte, error) {
	if c == nil {
		return nil, errors.New("nil cancel")
	}
	cc := proto.Clone(c).(*seqobv1.OfferCancel)
	cc.Sig = nil
	return deterministic(cc)
}

// CancelHash is sha256 over the canonical cancel bytes.
func CancelHash(c *seqobv1.OfferCancel) ([32]byte, error) {
	b, err := CanonicalCancelBytes(c)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(b), nil
}

// SignCancel fills c.Sig with a DER secp256k1 signature over CancelHash(c).
// As with SignOffer, c.MakerPubkey is set from priv if empty, else must match.
func SignCancel(c *seqobv1.OfferCancel, priv *btcec.PrivateKey) error {
	if c == nil {
		return errors.New("nil cancel")
	}
	pubHex := hex.EncodeToString(priv.PubKey().SerializeCompressed())
	if c.MakerPubkey == "" {
		c.MakerPubkey = pubHex
	} else if !strings.EqualFold(c.MakerPubkey, pubHex) {
		return fmt.Errorf("maker_pubkey %q does not match signing key %q", c.MakerPubkey, pubHex)
	}
	h, err := CancelHash(c)
	if err != nil {
		return err
	}
	c.Sig = ecdsa.Sign(priv, h[:]).Serialize()
	return nil
}

// VerifyCancel checks c.Sig against c.MakerPubkey over CancelHash(c).
func VerifyCancel(c *seqobv1.OfferCancel) error {
	if c == nil {
		return errors.New("nil cancel")
	}
	if len(c.Sig) == 0 {
		return errors.New("missing cancel sig")
	}
	pub, err := parsePubkeyHex(c.MakerPubkey)
	if err != nil {
		return err
	}
	sig, err := parseCanonicalSig(c.Sig, "cancel sig")
	if err != nil {
		return err
	}
	h, err := CancelHash(c)
	if err != nil {
		return err
	}
	if !sig.Verify(h[:], pub) {
		return errors.New("cancel sig verification failed")
	}
	return nil
}

// CanonicalSettledTradeBytes returns the deterministic encoding of st with sig
// cleared. The signed payload is {offer_id, maker_pubkey, fill_base, settle_txid,
// anchor_confs, nonce}; the nonce defeats replay of an old settled-trade against a
// re-posted same offer_id (which -requote reuses). Unlike a session ack, this record
// is keyed to the offer and stands on its own signature, so the relay can attribute a
// cross/LN settlement even after the courier session is gone.
func CanonicalSettledTradeBytes(st *seqobv1.SettledTrade) ([]byte, error) {
	if st == nil {
		return nil, errors.New("nil settled_trade")
	}
	cc := proto.Clone(st).(*seqobv1.SettledTrade)
	cc.Sig = nil
	return deterministic(cc)
}

// SettledTradeHash is sha256 over the canonical settled-trade bytes.
func SettledTradeHash(st *seqobv1.SettledTrade) ([32]byte, error) {
	b, err := CanonicalSettledTradeBytes(st)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(b), nil
}

// SignSettledTrade fills st.Sig with a DER secp256k1 signature over
// SettledTradeHash(st). As with SignOffer, st.MakerPubkey is set from priv if empty,
// else must match priv's compressed public key.
func SignSettledTrade(st *seqobv1.SettledTrade, priv *btcec.PrivateKey) error {
	if st == nil {
		return errors.New("nil settled_trade")
	}
	pubHex := hex.EncodeToString(priv.PubKey().SerializeCompressed())
	if st.MakerPubkey == "" {
		st.MakerPubkey = pubHex
	} else if !strings.EqualFold(st.MakerPubkey, pubHex) {
		return fmt.Errorf("maker_pubkey %q does not match signing key %q", st.MakerPubkey, pubHex)
	}
	h, err := SettledTradeHash(st)
	if err != nil {
		return err
	}
	st.Sig = ecdsa.Sign(priv, h[:]).Serialize()
	return nil
}

// VerifySettledTrade checks st.Sig against st.MakerPubkey over SettledTradeHash(st).
// The relay additionally keys the record to (maker_pubkey, offer_id), so a valid
// signature proves only the offer's own maker can record/decrement its fill.
func VerifySettledTrade(st *seqobv1.SettledTrade) error {
	if st == nil {
		return errors.New("nil settled_trade")
	}
	if len(st.Sig) == 0 {
		return errors.New("missing settled_trade sig")
	}
	pub, err := parsePubkeyHex(st.MakerPubkey)
	if err != nil {
		return err
	}
	sig, err := parseCanonicalSig(st.Sig, "settled_trade sig")
	if err != nil {
		return err
	}
	h, err := SettledTradeHash(st)
	if err != nil {
		return err
	}
	if !sig.Verify(h[:], pub) {
		return errors.New("settled_trade sig verification failed")
	}
	return nil
}

// ReattachHash is the sha256 challenge a participant signs to re-bind a
// reconnected WebSocket to a live lift session (P3.8). Binding the session_id AND
// the role prevents replaying a maker's signature as a taker (or across sessions).
func ReattachHash(sessionID, role string) [32]byte {
	return sha256.Sum256([]byte("seqob-reattach|" + sessionID + "|" + role))
}

// SignReattach returns a DER secp256k1 signature over ReattachHash(sessionID, role)
// with priv — the session participant's key (the maker's offer key, or the taker's
// ephemeral session key).
func SignReattach(sessionID, role string, priv *btcec.PrivateKey) []byte {
	h := ReattachHash(sessionID, role)
	return ecdsa.Sign(priv, h[:]).Serialize()
}

// VerifyReattach checks a DER secp256k1 signature over ReattachHash(sessionID, role)
// against the compressed session-key bytes that the relay already holds for that role.
func VerifyReattach(sessionID, role string, pubkey, sig []byte) error {
	if len(sig) == 0 {
		return errors.New("missing reattach sig")
	}
	pub, err := btcec.ParsePubKey(pubkey)
	if err != nil {
		return fmt.Errorf("bad session pubkey: %w", err)
	}
	s, err := parseCanonicalSig(sig, "reattach sig")
	if err != nil {
		return err
	}
	h := ReattachHash(sessionID, role)
	if !s.Verify(h[:], pub) {
		return errors.New("reattach sig verification failed")
	}
	return nil
}

// parseCanonicalSig parses a DER signature and accepts only its one canonical
// encoding: minimal DER with S in the low half of the curve order. secp256k1
// ECDSA is malleable — (r, s) and (r, N-s) verify equally — and the relay keys
// defences on the signature BYTES (the no-budget replay gate, who may edit or
// re-own a resting offer), so a non-canonical variant of a public signature
// must never count as a different, fresh signature. Serialize re-encodes
// minimally with S normalised, so any difference from the input is non-canonical.
func parseCanonicalSig(der []byte, what string) (*ecdsa.Signature, error) {
	sig, err := ecdsa.ParseDERSignature(der)
	if err != nil {
		return nil, fmt.Errorf("bad %s encoding: %w", what, err)
	}
	if !bytes.Equal(sig.Serialize(), der) {
		return nil, fmt.Errorf("non-canonical %s (high-S or non-minimal DER)", what)
	}
	return sig, nil
}

func parsePubkeyHex(s string) (*btcec.PublicKey, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("bad maker_pubkey hex: %w", err)
	}
	pub, err := btcec.ParsePubKey(b)
	if err != nil {
		return nil, fmt.Errorf("bad maker_pubkey: %w", err)
	}
	return pub, nil
}
