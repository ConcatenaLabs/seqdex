package offerstore

// The covenant book file keeps a relay's covenant orders across a restart.
//
// Only covenant orders are kept. Each is the maker's signed offer (public text)
// pointing at a coin on the chain, and the maker is offline by design, so after
// a restart nobody may be there to post it again. Everything in the file is
// checked again when it is read back (the signature, the terms, the expiry and
// the outpoint on the chain), so a stale line can only be dropped, never listed.
// Interactive offers are not kept: their makers post them again on reconnect,
// and keeping one whose maker is gone would list an order nobody can fill.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"google.golang.org/protobuf/encoding/protojson"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
)

// KeptCovenant is one covenant order as the covenant book file keeps it: the
// signed offer at the outpoint it rests at, the size it was listed at, and
// whether that outpoint is a remainder (Entry.reRested).
type KeptCovenant struct {
	Offer    *seqobv1.Offer
	Active   uint64
	ReRested bool
}

type keptLine struct {
	Offer    json.RawMessage `json:"offer"`
	Active   uint64          `json:"active"`
	ReRested bool            `json:"re_rested,omitempty"`
}

var keptJSON = protojson.MarshalOptions{UseProtoNames: true}

// CovenantBookBytes is the covenant book file's contents for the covenant
// orders resting now: one JSON line per order, in key order, so equal books
// give equal bytes.
func (s *Store) CovenantBookBytes() ([]byte, error) {
	covs := s.SnapshotCovenants()
	sort.Slice(covs, func(i, j int) bool {
		if covs[i].Key.MakerPubkey != covs[j].Key.MakerPubkey {
			return covs[i].Key.MakerPubkey < covs[j].Key.MakerPubkey
		}
		return covs[i].Key.OfferID < covs[j].Key.OfferID
	})
	var buf bytes.Buffer
	for _, c := range covs {
		o, err := keptJSON.Marshal(c.Offer)
		if err != nil {
			return nil, err
		}
		line, err := json.Marshal(keptLine{Offer: o, Active: c.Active, ReRested: c.ReRested})
		if err != nil {
			return nil, err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}

// CovenantBookWriter writes the covenant book file whenever the covenant
// orders change.
type CovenantBookWriter struct {
	Path string
	last []byte
}

// Flush writes the file if the book's covenant orders differ from what it last
// wrote: to a temporary file beside it, synced, then renamed over it, so a
// crash leaves the old file or the new one, never half of one.
func (w *CovenantBookWriter) Flush(s *Store) (bool, error) {
	b, err := s.CovenantBookBytes()
	if err != nil {
		return false, err
	}
	if w.last != nil && bytes.Equal(b, w.last) {
		return false, nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(w.Path), filepath.Base(w.Path)+".*.tmp")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tmp.Name(), w.Path); err != nil {
		return false, err
	}
	w.last = b
	return true, nil
}

// ReadCovenantBook reads a covenant book file. A missing file is an empty
// book. A line that does not parse, or is not a covenant offer, is reported in
// bad and skipped; the caller checks every kept order again before listing it.
func ReadCovenantBook(path string) (kept []KeptCovenant, bad []error, err error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for n := 1; sc.Scan(); n++ {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var l keptLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			bad = append(bad, fmt.Errorf("line %d: %v", n, err))
			continue
		}
		var o seqobv1.Offer
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(l.Offer, &o); err != nil {
			bad = append(bad, fmt.Errorf("line %d: offer: %v", n, err))
			continue
		}
		if !offer.IsCovenant(&o) {
			bad = append(bad, fmt.Errorf("line %d: offer %s is not a covenant order", n, o.GetOfferId()))
			continue
		}
		kept = append(kept, KeptCovenant{Offer: &o, Active: l.Active, ReRested: l.ReRested})
	}
	return kept, bad, sc.Err()
}

// RestoreCovenant puts a kept covenant order back in the book after a restart,
// once the caller has checked it again: at listed atoms, its kept re-rested
// mark, or, when its outpoint was spent while the relay was down (spent),
// held at 0 for the chain watcher to follow the spend (to the remainder a part
// fill made, or out of the book). listed is clamped to the signed size.
func (s *Store) RestoreCovenant(o *seqobv1.Offer, listed uint64, reRested, spent bool) (Key, error) {
	if !offer.IsCovenant(o) {
		return Key{}, fmt.Errorf("offer %s is not a covenant order", o.GetOfferId())
	}
	if listed == 0 || listed > o.GetBaseAmount() {
		listed = o.GetBaseAmount()
	}
	k, err := s.SubmitAt(o, listed)
	if err != nil {
		return k, err
	}
	s.mu.Lock()
	if e, ok := s.entries[k]; ok {
		e.reRested = e.reRested || reRested
	}
	s.mu.Unlock()
	if spent {
		return k, s.HoldCovenantForSpend(k, "")
	}
	return k, nil
}
