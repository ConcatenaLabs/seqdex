package offerstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
)

// The covenant book file keeps the covenant orders (at their current outpoint,
// listed size and re-rested mark), never an interactive offer; it is rewritten
// only when they change; and what it keeps is put back as it was.
func TestCovenantBookKeepsTheCovenantOrders(t *testing.T) {
	s := New(nil)
	k := key(t)
	if _, err := s.SubmitAt(mkCovOffer(t, k, "rem", "aabb", 1, 90), 37); err != nil {
		t.Fatal(err)
	}
	whole := mkCovOffer(t, k, "whole", "ccdd", 0, 90)
	if _, err := s.Submit(whole); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(mkOffer(t, k, "plain", uint64(time.Now().Add(time.Hour).Unix()))); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "covenants.jsonl")
	w := &CovenantBookWriter{Path: path}
	if wrote, err := w.Flush(s); err != nil || !wrote {
		t.Fatalf("first flush: %v %v", wrote, err)
	}
	if wrote, _ := w.Flush(s); wrote {
		t.Fatal("rewrote an unchanged book")
	}
	b, _ := os.ReadFile(path)
	if n := strings.Count(string(b), "\n"); n != 2 || strings.Contains(string(b), "plain") {
		t.Fatalf("file holds %d lines:\n%s", n, b)
	}
	// The watcher re-rests the whole one at a remainder: the file follows.
	kw := Key{MakerPubkey: whole.GetMakerPubkey(), OfferID: "whole"}
	if err := s.RepointCovenant(kw, "eeff", 1, 50, true); err != nil {
		t.Fatal(err)
	}
	if wrote, _ := w.Flush(s); !wrote {
		t.Fatal("a re-rest did not rewrite the book")
	}
	if left, _ := filepath.Glob(path + ".*.tmp"); len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}

	kept, bad, err := ReadCovenantBook(path)
	if err != nil || len(bad) != 0 || len(kept) != 2 {
		t.Fatalf("read: %d kept, bad %v, %v", len(kept), bad, err)
	}
	s2 := New(nil)
	for _, kc := range kept {
		if err := offer.VerifyOffer(kc.Offer); err != nil {
			t.Fatalf("%s no longer verifies: %v", kc.Offer.GetOfferId(), err)
		}
		if _, err := s2.RestoreCovenant(kc.Offer, kc.Active, kc.ReRested, false); err != nil {
			t.Fatal(err)
		}
	}
	for id, want := range map[string]struct {
		active uint64
		txid   string
	}{"rem": {37, "aabb"}, "whole": {50, "eeff"}} {
		e, ok := s2.Get(Key{MakerPubkey: whole.GetMakerPubkey(), OfferID: id})
		if !ok || e.ActiveAmount != want.active || !e.reRested || e.Offer.GetCovenant().GetCovenantTxid() != want.txid {
			t.Fatalf("%s restored as %+v", id, e)
		}
	}
	// A spent one is put back held at 0, for the watcher to follow.
	s3 := New(nil)
	kh, err := s3.RestoreCovenant(kept[0].Offer, kept[0].Active, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := s3.Get(kh); e.ActiveAmount != 0 || e.Status != seqobv1.OfferStatus_OFFER_STATUS_PARTIAL {
		t.Fatalf("spent restored as %+v", e)
	}
	if _, err := s3.RestoreCovenant(mkOffer(t, k, "plain", uint64(time.Now().Add(time.Hour).Unix())), 0, false, false); err == nil {
		t.Fatal("an interactive offer was restored")
	}
}

// A damaged or foreign line is skipped and named; a missing file is an empty book.
func TestReadCovenantBookSkipsWhatItCannotRead(t *testing.T) {
	if kept, bad, err := ReadCovenantBook(filepath.Join(t.TempDir(), "none")); err != nil || kept != nil || bad != nil {
		t.Fatalf("missing file: %v %v %v", kept, bad, err)
	}
	s := New(nil)
	k := key(t)
	if _, err := s.Submit(mkCovOffer(t, k, "c", "aabb", 0, 90)); err != nil {
		t.Fatal(err)
	}
	good, _ := s.CovenantBookBytes()
	path := filepath.Join(t.TempDir(), "covenants.jsonl")
	plain := `{"offer":{"offer_id":"plain","same_chain":{}},"active":5}`
	if err := os.WriteFile(path, []byte("not json\n"+plain+"\n\n"+string(good)), 0o600); err != nil {
		t.Fatal(err)
	}
	kept, bad, err := ReadCovenantBook(path)
	if err != nil || len(kept) != 1 || len(bad) != 2 || !strings.Contains(bad[1].Error(), "not a covenant order") {
		t.Fatalf("kept %d bad %v err %v", len(kept), bad, err)
	}
}
