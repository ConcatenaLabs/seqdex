package xchain

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"
)

// Env-gated LIVE test of the hold rules on a node whose channels hold two assets.
// The payer has a channel in each asset to the holder, which runs the
// holdinvoice-seq plugin. The payer may be a keyless node: every bare-hash payment
// asks its device for approval first.
//
//	SEQLN_ASSET_SOCK_PAYER = payer node lightning-rpc
//	SEQLN_ASSET_SOCK_PAYEE = holder node lightning-rpc (holdinvoice-seq loaded)
//	SEQLN_ASSET_ID         = the asset the holds are made in (GOLD)
//	SEQLN_OTHER_ASSET_ID   = a second asset with a channel between the two (SILV)
//	SEQLN_OVER_LIMIT_MSAT  = optional: an amount over the payer device's limit
//
//	go test ./pkg/xchain -run TestLNLegHoldAssetLive -v
func TestLNLegHoldAssetLive(t *testing.T) {
	payerSock, payeeSock, gold, amt := assetLegEnv(t)
	silv := os.Getenv("SEQLN_OTHER_ASSET_ID")
	if silv == "" {
		t.Skip("set SEQLN_OTHER_ASSET_ID")
	}
	holdGold := NewCLNAssetLNLeg(payeeSock, gold)
	holdSilv := NewCLNAssetLNLeg(payeeSock, silv)
	payGold := NewCLNAssetLNLeg(payerSock, gold)
	paySilv := NewCLNAssetLNLeg(payerSock, silv)
	holderID, err := holdGold.NodeID()
	if err != nil {
		t.Fatalf("holder NodeID: %v", err)
	}
	fresh := func() ([]byte, []byte) {
		p := make([]byte, 32)
		_, _ = rand.Read(p)
		h := sha256.Sum256(p)
		return p, h[:]
	}
	lookup := func(h []byte) map[string]interface{} {
		var res map[string]interface{}
		if err := holdGold.rpc.call(&res, "holdinvoicelookup", map[string]interface{}{"payment_hash": hex.EncodeToString(h)}); err != nil {
			t.Fatalf("holdinvoicelookup: %v", err)
		}
		return res
	}

	// 0. A leg that names no asset makes no hold on this node.
	_, h0 := fresh()
	if _, err := NewCLNLNLeg(payeeSock).CreateHoldInvoice(h0, amt, 0, "l09-noasset", "l09"); !errors.Is(err, ErrLNHoldAsset) {
		t.Fatalf("hold with no asset: err = %v, want ErrLNHoldAsset", err)
	} else {
		t.Logf("no-asset hold refused: %v", err)
	}
	if st := lookup(h0)["state"]; st != "unknown" {
		t.Fatalf("a hold was created without an asset: state %v", st)
	}

	// 1. A GOLD hold paid in SILV: the plugin refuses the HTLC, the hold never
	//    leaves waiting, the holder never settles, the payer learns nothing.
	_, h1 := fresh()
	if _, err := holdGold.CreateHoldInvoice(h1, amt, 0, "l09-h1", "l09"); err != nil {
		t.Fatalf("GOLD hold: %v", err)
	}
	pre1, perr := paySilv.PayHash(holderID, h1, amt, 18, nil)
	t.Logf("SILV payment of a GOLD hold: preimage %x, err %v", pre1, perr)
	if perr == nil || pre1 != nil {
		t.Fatal("a SILV payment of a GOLD hold went through")
	}
	if _, err := holdGold.WaitHeldInfo(h1, 3*time.Second); !errors.Is(err, ErrLNLegTimeout) {
		t.Fatalf("WaitHeld on the GOLD hold after a SILV HTLC: err = %v, want a timeout", err)
	}
	l1 := lookup(h1)
	t.Logf("holdinvoicelookup after the SILV HTLC: %v", l1)
	if l1["state"] != "waiting" || l1["received_msat"].(float64) != 0 {
		t.Fatalf("the GOLD hold moved: %v", l1)
	}
	_ = holdGold.CancelHold(h1)

	// 2. A hold accepted in SILV, waited on by a GOLD leg: refused, cancelled, the
	//    payer's HTLC fails back and the preimage stays withheld.
	_, h2 := fresh()
	if _, err := holdSilv.CreateHoldInvoice(h2, amt, 0, "l09-h2", "l09"); err != nil {
		t.Fatalf("SILV hold: %v", err)
	}
	payDone := make(chan error, 1)
	go func() { _, err := paySilv.PayHash(holderID, h2, amt, 18, nil); payDone <- err }()
	_, werr := holdGold.WaitHeldInfo(h2, 60*time.Second)
	t.Logf("GOLD leg waiting on a SILV hold: %v", werr)
	if !errors.Is(werr, ErrLNHoldAsset) {
		t.Fatalf("err = %v, want ErrLNHoldAsset", werr)
	}
	if err := holdGold.CancelHold(h2); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if err := <-payDone; err == nil {
		t.Fatal("the payer's HTLC for the refused hold settled")
	} else {
		t.Logf("payer after the refusal: %v", err)
	}
	if st := lookup(h2)["state"]; st != "cancelled" {
		t.Fatalf("hold h2 state %v, want cancelled", st)
	}

	// 3. The right asset: held in GOLD, checked, settled; the payer learns P.
	p3, h3 := fresh()
	if _, err := holdGold.CreateHoldInvoice(h3, amt, 0, "l09-h3", "l09"); err != nil {
		t.Fatalf("GOLD hold: %v", err)
	}
	settled := make(chan error, 1)
	go func() {
		info, err := holdGold.WaitHeldInfo(h3, 60*time.Second)
		if err != nil {
			settled <- err
			return
		}
		t.Logf("held: %+v", info)
		if info.Asset != gold || info.ReceivedMsat != amt {
			settled <- errors.New("held in the wrong asset or amount")
			return
		}
		settled <- holdGold.SettleHold(h3, p3)
	}()
	pre3, err := payGold.PayHash(holderID, h3, amt, 18, nil)
	if err != nil {
		t.Fatalf("GOLD PayHash: %v", err)
	}
	if err := <-settled; err != nil {
		t.Fatalf("holder: %v", err)
	}
	if hex.EncodeToString(pre3) != hex.EncodeToString(p3) {
		t.Fatalf("payer learned %x, want %x", pre3, p3)
	}
	t.Logf("GOLD hold held and settled; payer learned the preimage")

	// 4. Over the payer device's limit: declined before any HTLC exists.
	if v := os.Getenv("SEQLN_OVER_LIMIT_MSAT"); v != "" {
		var over uint64
		for _, c := range v {
			over = over*10 + uint64(c-'0')
		}
		_, h4 := fresh()
		if _, err := holdGold.CreateHoldInvoice(h4, over, 0, "l09-h4", "l09"); err != nil {
			t.Fatalf("GOLD hold: %v", err)
		}
		_, err := payGold.PayHash(holderID, h4, over, 18, nil)
		t.Logf("over the limit: %v", err)
		if !errors.Is(err, ErrLNPayDeclined) {
			t.Fatalf("err = %v, want ErrLNPayDeclined", err)
		}
		var sp struct {
			Payments []interface{} `json:"payments"`
		}
		if err := payGold.rpc.call(&sp, "listsendpays", map[string]interface{}{"payment_hash": hex.EncodeToString(h4)}); err != nil {
			t.Fatalf("listsendpays: %v", err)
		}
		if len(sp.Payments) != 0 {
			t.Fatalf("a declined payment was sent: %v", sp.Payments)
		}
		if st := lookup(h4)["state"]; st != "waiting" {
			t.Fatalf("hold h4 state %v, want waiting", st)
		}
		_ = holdGold.CancelHold(h4)
	}
}
