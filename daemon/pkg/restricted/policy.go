// Package restricted is the Sequentia side of a trade of a restricted
// (OpenAMP) asset: the client of the asset's policy server (openampd), and the
// swap HTLC's lock, claim and refund transactions that a maker or a taker
// builds around it.
//
// Every policy-server answer is checked before anything is signed: a sighash
// the server returns is recomputed here and refused on a mismatch, and every
// script is rebuilt from the terms (pkg/covenant) rather than read from the
// server or the counterparty.
package restricted

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Policy is a client of one asset policy server (openampd).
type Policy struct {
	Base string // e.g. https://host/openamp, without a trailing slash
	HTTP *http.Client
}

// NewPolicy returns a client with a 30 s timeout.
func NewPolicy(base string) *Policy {
	return &Policy{Base: strings.TrimRight(base, "/"), HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// Refusal is a policy server's answer other than 200: its status, the stable
// code when it gave one, and its sentence.
type Refusal struct {
	Status int
	Code   string
	Reason string
}

func (r *Refusal) Error() string {
	if r.Code != "" {
		return fmt.Sprintf("policy server %d (%s): %s", r.Status, r.Code, r.Reason)
	}
	return fmt.Sprintf("policy server %d: %s", r.Status, r.Reason)
}

func (p *Policy) do(method, path string, body, out interface{}) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, p.Base+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(raw))
		}
		return &Refusal{Status: resp.StatusCode, Code: e.Code, Reason: e.Error}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// Asset is the part of a policy server's asset record a trade reads.
type Asset struct {
	ID          string `json:"id"` // display hex
	Ticker      string `json:"ticker"`
	Precision   int    `json:"precision"`
	PolicyPub   string `json:"policy_pub"`
	IssuerPub   string `json:"issuer_pub"`
	Clawback    bool   `json:"clawback"`
	Enforcement string `json:"enforcement,omitempty"`
}

// Keys returns the policy key and, where the asset elected clawback, the
// issuer key.
func (a *Asset) Keys() (policy [32]byte, issuer *[32]byte, err error) {
	if policy, err = hex32(a.PolicyPub); err != nil {
		return policy, nil, fmt.Errorf("asset %s policy key: %w", a.ID, err)
	}
	if a.Clawback {
		x, err := hex32(a.IssuerPub)
		if err != nil {
			return policy, nil, fmt.Errorf("asset %s issuer key: %w", a.ID, err)
		}
		issuer = &x
	}
	return policy, issuer, nil
}

// Asset reads an asset's record (id in display hex).
func (p *Policy) Asset(id string) (*Asset, error) {
	var a Asset
	if err := p.do("GET", "/v1/assets/"+url.PathEscape(id), nil, &a); err != nil {
		return nil, err
	}
	if a.Enforcement != "" && a.Enforcement != "cosign" {
		return nil, fmt.Errorf("asset %s is enforced by %q, not by its policy server's co-signature", id, a.Enforcement)
	}
	return &a, nil
}

// UserKey returns an account's active enclave key (its first).
func (p *Policy) UserKey(aid string) ([32]byte, error) {
	var u struct {
		Pubkeys []string `json:"pubkeys"`
		Frozen  bool     `json:"frozen"`
	}
	if err := p.do("GET", "/v1/users/"+url.PathEscape(aid), nil, &u); err != nil {
		return [32]byte{}, err
	}
	if len(u.Pubkeys) == 0 {
		return [32]byte{}, fmt.Errorf("account %s has no key", aid)
	}
	return hex32(u.Pubkeys[0])
}

// EnclaveScript returns an account's enclave scriptPubKey for an asset.
func (p *Policy) EnclaveScript(aid, asset string) ([]byte, error) {
	var a struct {
		ScriptPubKey string `json:"script_pubkey"`
	}
	q := url.Values{"asset": {asset}}
	if err := p.do("GET", "/v1/users/"+url.PathEscape(aid)+"/address?"+q.Encode(), nil, &a); err != nil {
		return nil, err
	}
	spk, err := hex.DecodeString(a.ScriptPubKey)
	if err != nil || len(spk) != 34 || spk[0] != 0x51 || spk[1] != 0x20 {
		return nil, fmt.Errorf("account %s: enclave script %q is not a v1 program", aid, a.ScriptPubKey)
	}
	return spk, nil
}

// Onboarding is where a party not yet approved asks to be.
type Onboarding struct {
	Name string `json:"name,omitempty"`
	URL  string `json:"url,omitempty"`
}

// Eligibility is GET /v1/eligibility's answer.
type Eligibility struct {
	Eligible           bool        `json:"eligible"`
	Code               string      `json:"code,omitempty"`
	Reason             string      `json:"reason,omitempty"`
	Onboarding         *Onboarding `json:"onboarding,omitempty"`
	OperatorOnboarding *Onboarding `json:"operator_onboarding,omitempty"`
}

// Eligible asks whether aid may receive atoms of asset now, from the sender
// from (an account, or "" for nobody in particular). Nothing is signed or
// recorded.
func (p *Policy) Eligible(asset, aid, from string, atoms uint64) (*Eligibility, error) {
	q := url.Values{"asset": {asset}, "aid": {aid}, "atoms": {fmt.Sprint(atoms)}}
	if from != "" {
		q.Set("from", from)
	}
	var e Eligibility
	if err := p.do("GET", "/v1/eligibility?"+q.Encode(), nil, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// HTLCOutput names a lock's HTLC output.
type HTLCOutput struct {
	Output   int    `json:"output"`
	BuyerAID string `json:"buyer_aid"`
	Hash     string `json:"hash"`
	TSeq     uint32 `json:"t_seq"`
}

// OrderOutput names a gated order's output in its placement: the fill
// covenant it wraps (hex) and its expiry (a Unix time).
type OrderOutput struct {
	Output int    `json:"output"`
	Fill   string `json:"fill"`
	Expiry uint32 `json:"expiry"`
}

// OrderInput names a fill's gated order input.
type OrderInput struct {
	Input int `json:"input"`
}

// HTLCInput names a claim's HTLC input.
type HTLCInput struct {
	Input int `json:"input"`
}

// CosignRequest is POST /v1/cosign's body (and /v1/check's).
type CosignRequest struct {
	Tx          string       `json:"tx"`
	Asset       string       `json:"asset"`
	SenderAID   string       `json:"sender_aid"`
	Inputs      []int        `json:"inputs,omitempty"`
	SenderSig   string       `json:"sender_sig,omitempty"`
	HTLCOutputs []HTLCOutput `json:"htlc_outputs,omitempty"`
	HTLCInputs  []HTLCInput  `json:"htlc_inputs,omitempty"`
	// OrderOutputs names a gated order's placement outputs.
	OrderOutputs []OrderOutput `json:"order_outputs,omitempty"`
	// OrderInputs names a gated order's input in a fill.
	OrderInputs []OrderInput `json:"order_inputs,omitempty"`

	Broadcast bool              `json:"broadcast,omitempty"`
	UserSigs  map[string]string `json:"user_sigs,omitempty"`
	ExpiresAt int64             `json:"expires_at,omitempty"`
	ExpirySig string            `json:"expiry_sig,omitempty"`
}

// CosignSig is one input's policy signature.
type CosignSig struct {
	Input     int    `json:"input"`
	Sighash   string `json:"sighash"`
	PolicySig string `json:"policy_sig"`
	Leaf      string `json:"leaf"`
	Control   string `json:"control"`
}

// CosignResponse is /v1/cosign's answer.
type CosignResponse struct {
	Sigs   []CosignSig `json:"sigs"`
	Cutoff int64       `json:"cutoff,omitempty"`
	Txid   string      `json:"txid,omitempty"`
}

// Cosign asks for the policy signatures of a transaction.
func (p *Policy) Cosign(req CosignRequest) (*CosignResponse, error) {
	var r CosignResponse
	if err := p.do("POST", "/v1/cosign", req, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Check judges a transaction as Cosign would, signing and recording nothing.
func (p *Policy) Check(req CosignRequest) error {
	return p.do("POST", "/v1/check", req, nil)
}

func hex32(s string) ([32]byte, error) {
	var out [32]byte
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return out, fmt.Errorf("%q is not 32 bytes of hex", s)
	}
	copy(out[:], b)
	return out, nil
}
