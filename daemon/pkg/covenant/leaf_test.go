package covenant

import (
	"encoding/hex"
	"strings"
	"testing"
)

// Golden vector generated from the PROVEN Python builders
// (test/functional/seqob_covenant.py in the Sequentia tree), on the
// fixed inputs below. This pins the Go production covenant module byte-for-byte
// to the audited artifact that feature_seqob_covenant_fill.py exercises with 11
// consensus scenarios: if this test passes, the FILL/REFUND leaf bytes, the
// taptree hashes, the tweaked output key, and the control block are identical.
//
//	asset_a = bytes(range(0,32)); asset_b = bytes(range(32,64))
//	rate_num=3, rate_den=7, min_lot=500000000
//	maker_prog = 0x11*32, expiry=400, maker_x = 0x22*32, internal_key = NUMS
const (
	goldFillLeaf   = "cdc95188cd76938bd59f63cd76938bce518820000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f8763cd76938bd1cdca7b8888cd76938bcf518876080065cd1d00000000df6967080000000000000000686708000000000000000068d86976080065cd1d00000000df69080300000000000000d969080600000000000000d769080700000000000000da6977cd7693ce518820202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f88cd7693d1518820111111111111111111111111111111111111111111111111111111111111111188cd7693cf51887cdf"
	goldRefundLeaf = "029001b175202222222222222222222222222222222222222222222222222222222222222222ac"
	goldFillHash   = "4d1a21f10826870560f07e69520416221c532fb8744bc0a0b2bf38032cd03343"
	goldRefundHash = "b36045e27b7a5812d8d7339811db86ef98751c7e382a84a1d34949a83b4ae920"
	goldMerkleRoot = "224a2849e4419e6a81593dad51e14766083c08e31081f0aecaea3d1dc7daf3c3"
	goldOutputKey  = "b22544534c99090050a06eece12231a2321f4144661ab3964408d5780821afaa"
	goldSPK        = "5120b22544534c99090050a06eece12231a2321f4144661ab3964408d5780821afaa"
	goldNegflag    = 1
	goldCtrlBlock  = "c550929b74c1a04954b78b4b6035e97a5e078a5a0f28ec96d547bfee9ace803ac0b36045e27b7a5812d8d7339811db86ef98751c7e382a84a1d34949a83b4ae920"
)

func fixedOrder() Order {
	var a, b, mx [32]byte
	for i := 0; i < 32; i++ {
		a[i] = byte(i)
		b[i] = byte(32 + i)
		mx[i] = 0x22
	}
	prog := make([]byte, 32)
	for i := range prog {
		prog[i] = 0x11
	}
	return DefaultOrder(a, b, 3, 7, 500000000, prog, 400, mx)
}

func TestFillLeafBytesMatchPython(t *testing.T) {
	o := fixedOrder()
	fill, err := BuildFillLeaf(o.AssetA, o.AssetB, o.RateNum, o.RateDen, o.MinLot, o.MakerProg, o.MakerVer)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(fill); got != goldFillLeaf {
		t.Fatalf("FILL leaf mismatch\n got %s\nwant %s", got, goldFillLeaf)
	}
}

func TestRefundLeafBytesMatchPython(t *testing.T) {
	o := fixedOrder()
	refund, err := BuildRefundLeaf(o.ExpiryLocktime, o.MakerX)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(refund); got != goldRefundLeaf {
		t.Fatalf("REFUND leaf mismatch\n got %s\nwant %s", got, goldRefundLeaf)
	}
}

func TestTaptreeMatchesPython(t *testing.T) {
	tap, err := fixedOrder().Derive()
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name, got, want string
	}{
		{"fill_leaf_hash", hex.EncodeToString(tap.FillLeafHash[:]), goldFillHash},
		{"refund_leaf_hash", hex.EncodeToString(tap.RefundLeafHash[:]), goldRefundHash},
		{"fill_merkle_branch", hex.EncodeToString(tap.FillMerkleBranch), goldRefundHash},
		{"merkle_root", hex.EncodeToString(tap.MerkleRoot[:]), goldMerkleRoot},
		{"output_key", hex.EncodeToString(tap.OutputKey[:]), goldOutputKey},
		{"scriptpubkey", hex.EncodeToString(tap.ScriptPubKey), goldSPK},
		{"control_block", hex.EncodeToString(tap.ControlBlock(NUMS)), goldCtrlBlock},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s mismatch\n got %s\nwant %s", c.name, c.got, c.want)
		}
	}
	if int(tap.Negflag) != goldNegflag {
		t.Errorf("negflag = %d, want %d", tap.Negflag, goldNegflag)
	}
}

func TestVerifyAgainstSPK(t *testing.T) {
	o := fixedOrder()
	spk, _ := hex.DecodeString(goldSPK)
	if _, err := o.VerifyAgainstSPK(spk, false); err != nil {
		t.Fatalf("verify against correct spk failed: %v", err)
	}
	// A one-byte-tampered spk must be rejected.
	bad := append([]byte{}, spk...)
	bad[len(bad)-1] ^= 0x01
	if _, err := o.VerifyAgainstSPK(bad, false); err == nil {
		t.Fatal("verify accepted a tampered spk")
	}
	// A non-NUMS internal key must be rejected unless explicitly allowed.
	o2 := o
	o2.InternalKey = o.MakerX // any non-NUMS x-only
	if _, err := o2.Derive(); err == nil {
		// derive may still succeed; the guard is in VerifyAgainstSPK.
		if _, err := o2.VerifyAgainstSPK(spk, false); err == nil {
			t.Fatal("verify accepted a non-NUMS (maker-cancellable) key without opt-in")
		}
	}
}

func TestCeilPrice(t *testing.T) {
	// ceil(90*1/3) = 30 exact; ceil(10*1/3) = 4 (rounds up).
	if CeilPrice(90, 1, 3) != 30 {
		t.Fatal("ceil price exact wrong")
	}
	if CeilPrice(10, 1, 3) != 4 {
		t.Fatal("ceil price round-up wrong")
	}
}

// A median-time REFUND lock is pushed as CLTV's number, as a height is: 1792611631
// (0x6ad9152f) is the 4-byte little-endian push 2f15d96a.
func TestRefundLeafTakesAMedianTime(t *testing.T) {
	var mx [32]byte
	for i := range mx {
		mx[i] = 0x22
	}
	leaf, err := BuildRefundLeaf(1792611631, mx)
	if err != nil {
		t.Fatal(err)
	}
	want := "042f15d96ab17520" + strings.Repeat("22", 32) + "ac"
	if got := hex.EncodeToString(leaf); got != want {
		t.Fatalf("refund leaf\n got %s\nwant %s", got, want)
	}
}

func TestRefundOpen(t *testing.T) {
	h := Order{ExpiryLocktime: 400}
	for _, c := range []struct {
		tip        int64
		open, know bool
	}{{399, false, true}, {400, true, true}} {
		if open, known := h.RefundOpen(c.tip, 0); open != c.open || known != c.know {
			t.Fatalf("height lock at tip %d: open %v known %v", c.tip, open, known)
		}
	}
	tl := Order{ExpiryLocktime: 1792611631}
	if !tl.RefundIsTime() || h.RefundIsTime() {
		t.Fatal("lock kinds")
	}
	if open, known := tl.RefundOpen(9_000_000, 0); open || known {
		t.Fatal("a median-time lock judged without a median time")
	}
	if open, _ := tl.RefundOpen(1, 1792611631); open {
		t.Fatal("a median time equal to the lock has not passed it (BIP 113)")
	}
	if open, _ := tl.RefundOpen(1, 1792611632); !open {
		t.Fatal("a median time past the lock opens it")
	}
}
