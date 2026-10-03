package xchain

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strconv"
	"testing"
	"time"
)

// Env-gated LIVE tests for the asset-LN leg primitives (Step 2, M1). They need
// two running SeqLN-on-Sequentia nodes with an asset (e.g. GOLD) channel from
// the payer toward the payee, and the holdinvoice-seq plugin loaded on the payee
// for the PayHash test.
//
//	SEQLN_ASSET_SOCK_PAYER = payer node lightning-rpc (holds asset outbound)
//	SEQLN_ASSET_SOCK_PAYEE = payee node lightning-rpc (issues invoice / holds)
//	SEQLN_ASSET_ID         = 32-byte hex asset id (as displayed)
//	SEQLN_ASSET_AMT_MSAT   = amount in asset-atom-msat (default 100000000 = 0.001)
//
//	go test ./pkg/xchain -run 'TestLNLegAsset' -v
func assetLegEnv(t *testing.T) (payer, payee, asset string, amt uint64) {
	t.Helper()
	payer = os.Getenv("SEQLN_ASSET_SOCK_PAYER")
	payee = os.Getenv("SEQLN_ASSET_SOCK_PAYEE")
	asset = os.Getenv("SEQLN_ASSET_ID")
	if payer == "" || payee == "" || asset == "" {
		t.Skip("set SEQLN_ASSET_SOCK_PAYER, SEQLN_ASSET_SOCK_PAYEE, SEQLN_ASSET_ID")
	}
	amt = 100000000
	if v := os.Getenv("SEQLN_ASSET_AMT_MSAT"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			amt = n
		}
	}
	return
}

// TestLNLegAssetPayLive: the maker's asset leg pays an asset invoice and learns
// the preimage (the NORMAL-direction primitive, now in an issued asset).
func TestLNLegAssetPayLive(t *testing.T) {
	payerSock, payeeSock, asset, amt := assetLegEnv(t)

	var p [32]byte
	copy(p[:], []byte("m1-asset-pay-preimage-32bytes!!!"))
	h := sha256.Sum256(p[:])

	// Payee issues an invoice on P in the same asset.
	payee := NewCLNAssetLNLeg(payeeSock, asset)
	label := "m1-assetpay-" + hex.EncodeToString(h[:4])
	bolt11, err := payee.CreateInvoice(p[:], amt, 0, label, "m1 asset pay")
	if err != nil {
		t.Fatalf("payee CreateInvoice: %v", err)
	}

	// Payer's ASSET leg pays it, routing in the asset, and must recover P.
	payer := NewCLNAssetLNLeg(payerSock, asset)
	pre, err := payer.Pay(bolt11, h[:], amt)
	if err != nil {
		t.Fatalf("asset Pay: %v", err)
	}
	if hex.EncodeToString(pre) != hex.EncodeToString(p[:]) {
		t.Fatalf("recovered preimage %x != %x", pre, p[:])
	}
	t.Logf("asset Pay OK: routed %d msat in asset %s, recovered preimage", amt, asset[:12])
}

// TestLNLegPayHashLive: the taker's primitive for paying a counterparty's HOLD
// leg by bare hash (no invoice), returning the preimage the payee reveals on
// settle. Uses the holdinvoice-seq plugin on the payee.
func TestLNLegPayHashLive(t *testing.T) {
	payerSock, payeeSock, asset, amt := assetLegEnv(t)

	var p [32]byte
	copy(p[:], []byte("m1-payhash-preimage-is-32-bytes!"))
	h := sha256.Sum256(p[:])
	secret := make([]byte, 32)
	secret[0] = 0xa5

	payee := NewCLNAssetLNLeg(payeeSock, asset) // holds in the asset it is paid in
	payer := NewCLNAssetLNLeg(payerSock, asset)

	payeeID, err := payee.NodeID()
	if err != nil {
		t.Fatalf("payee NodeID: %v", err)
	}

	// Payee registers the bare hash to be HELD (no invoice, does not know P).
	if _, err := payee.CreateHoldInvoice(h[:], amt, 0, "m1-payhash", "m1 payhash"); err != nil {
		t.Fatalf("payee CreateHoldInvoice(register): %v", err)
	}

	// Concurrently: wait until the HTLC is held, then settle it by revealing P.
	settleErr := make(chan error, 1)
	go func() {
		if _, err := payee.WaitHeld(h[:], 60*time.Second); err != nil {
			settleErr <- err
			return
		}
		settleErr <- payee.SettleHold(h[:], p[:])
	}()

	// Payer pays the bare hash; blocks until the payee settles, then returns P.
	pre, err := payer.PayHash(payeeID, h[:], amt, 18, secret)
	if err != nil {
		t.Fatalf("PayHash: %v", err)
	}
	if err := <-settleErr; err != nil {
		t.Fatalf("payee WaitHeld/SettleHold: %v", err)
	}
	if hex.EncodeToString(pre) != hex.EncodeToString(p[:]) {
		t.Fatalf("PayHash recovered preimage %x != %x", pre, p[:])
	}
	t.Logf("PayHash OK: paid bare hash in asset %s, recovered preimage on settle", asset[:12])
}
