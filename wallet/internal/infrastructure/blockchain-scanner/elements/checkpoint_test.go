package elements_scanner

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/aejkcs50/seqdex/wallet/internal/core/domain"
	"github.com/aejkcs50/seqdex/wallet/internal/infrastructure/blockchain-scanner/scanner"
	"github.com/google/uuid"
)

// fakeSvc records where each watch starts and says how far it has scanned.
type fakeSvc struct {
	starts []uint32
	low    uint32
}

func (f *fakeSvc) Start() (<-chan scanner.Report, error) { return make(chan scanner.Report), nil }
func (f *fakeSvc) Stop()                                {}
func (f *fakeSvc) Watch(opts ...scanner.ScanRequestOption) {
	r := &scanner.ScanRequest{}
	for _, o := range opts {
		o(r)
	}
	f.starts = append(f.starts, r.StartHeight)
}
func (f *fakeSvc) WatchDescriptorWallet(uuid.UUID, string, []scanner.EventType, int) error { return nil }
func (f *fakeSvc) LowestUnscanned() (uint32, bool)                                       { return f.low, true }

func TestResumeHeight(t *testing.T) {
	saved := map[string]uint32{"a": 190000, "low": 5}
	if h := resumeHeight(saved, "a", 1000); h != 190000-checkpointMargin {
		t.Fatalf("resumes below its saved height by the margin: %d", h)
	}
	if h := resumeHeight(saved, "a", 189999); h != 189999 {
		t.Fatalf("never below its birth: %d", h)
	}
	if h := resumeHeight(saved, "new", 1000); h != 1000 {
		t.Fatalf("an account with no checkpoint starts at its birth: %d", h)
	}
	if h := resumeHeight(saved, "low", 0); h != 0 {
		t.Fatalf("a checkpoint within the margin of 0: %d", h)
	}
}

// Saved heights are read back by the next run, and every watch of the account resumes there: the
// opening ones and one set later (the wallet's addresses watched once it is unlocked).
func TestCheckpointsRoundTripAndResume(t *testing.T) {
	file := filepath.Join(t.TempDir(), "checkpoints.json")
	fa, fb := &fakeSvc{low: 190500}, &fakeSvc{low: 42}
	s := &service{args: ServiceArgs{CheckpointFile: file}, lock: &sync.RWMutex{},
		scanners: map[string]*scannerService{"a": {svc: fa}, "b": {svc: fb}}}
	s.saveCheckpoints()
	got := loadCheckpoints(file)
	if got["a"] != 190500 || got["b"] != 42 {
		t.Fatalf("read back: %v", got)
	}
	f := &fakeSvc{}
	sc := &scannerService{svc: f, startingBlockHeight: 1000, resumeFrom: resumeHeight(got, "a", 1000),
		blindingKeys: map[string][]byte{}, lock: &sync.RWMutex{}, log: func(string, ...interface{}) {}, warn: func(error, string, ...interface{}) {}}
	sc.watchAddresses([]domain.AddressInfo{{Script: "0014" + "11111111111111111111111111111111111111aa"}})
	sc.watchAddresses([]domain.AddressInfo{{Script: "0014" + "22222222222222222222222222222222222222bb"}})
	if len(f.starts) != 2 || f.starts[0] != 190500-checkpointMargin || f.starts[1] != 190500-checkpointMargin {
		t.Fatalf("every watch resumes from the saved height: %v", f.starts)
	}
}

// A scanner made before its account's birthday was known (by a caller that knows none: the
// coin channels at unlock, or an address derived before the account's creation event) takes the
// birthday when it comes, and watches from there, never from genesis; one made with a birthday
// keeps it. A checkpoint saved at 0 by such a scan does not pull it back to genesis either.
func TestAScannerTakesItsAccountsBirthday(t *testing.T) {
	f := &fakeSvc{}
	s := &service{lock: &sync.RWMutex{}, checkpoints: map[string]uint32{"late": 0},
		scanners: map[string]*scannerService{"late": {svc: f, blindingKeys: map[string][]byte{}, lock: &sync.RWMutex{},
			log: func(string, ...interface{}) {}, warn: func(error, string, ...interface{}) {}}}}
	sc := s.getOrCreateScanner("late", 191900)
	sc.watchAddresses([]domain.AddressInfo{{Script: "0014" + "33333333333333333333333333333333333333cc"}})
	if len(f.starts) != 1 || f.starts[0] != 191900 {
		t.Fatalf("a scanner made without a birthday watches from %v, not its birthday 191900", f.starts)
	}
	if sc.adoptBirthday(5, 5) || sc.startingBlockHeight != 191900 {
		t.Fatalf("a known birthday is kept: %d", sc.startingBlockHeight)
	}
}

func quietScanner(f *fakeSvc, birth, resume uint32) *scannerService {
	return &scannerService{svc: f, startingBlockHeight: birth, resumeFrom: resume,
		blindingKeys: map[string][]byte{}, lock: &sync.RWMutex{}, log: func(string, ...interface{}) {}, warn: func(error, string, ...interface{}) {}}
}

// An address the account derives while the wallet runs is watched from the tip less the margin:
// it has no history before it was derived. Watched from where the wallet started (as the
// addresses it held then are), each one derived made the account scan every block since the
// start again, and held its saved height back there, so a wallet up for days resumed days back.
func TestAnAddressDerivedLaterIsWatchedFromTheTip(t *testing.T) {
	f := &fakeSvc{low: 209401} // caught up
	s := &service{lock: &sync.RWMutex{}, checkpoints: map[string]uint32{"a": 199023},
		scanners: map[string]*scannerService{"a": quietScanner(f, 93800, 199011)},
		tip:      func() (uint32, error) { return 209400, nil }}
	s.WatchForAccount("a", 93800, []domain.AddressInfo{{Script: "0014" + "44444444444444444444444444444444444444dd"}})
	s.WatchNewAddresses("a", 93800, []domain.AddressInfo{{Script: "0014" + "55555555555555555555555555555555555555ee"}})
	if len(f.starts) != 2 || f.starts[0] != 199011 || f.starts[1] != 209400-checkpointMargin {
		t.Fatalf("held at the opening from 199011, derived later from the tip less the margin %d: %v", 209400-checkpointMargin, f.starts)
	}
	// an account still catching up (scanned to 205000): a derived address is watched from there,
	// so a payment made to it before this wallet derived it, after that height, is seen
	f.low = 205000
	s.WatchNewAddresses("a", 93800, []domain.AddressInfo{{Script: "0014" + "77777777777777777777777777777777777777aa"}})
	if f.starts[2] != 205000 {
		t.Fatalf("an account behind the tip watches a derived address from its scanned height 205000: %v", f.starts)
	}
	f.low = 209401
	// with no tip to read it falls back to where the opening watches started: never later than
	// the account has been scanned to, so nothing is missed
	s.tip = func() (uint32, error) { return 0, errors.New("node down") }
	s.WatchNewAddresses("a", 93800, []domain.AddressInfo{{Script: "0014" + "66666666666666666666666666666666666666ff"}})
	if f.starts[3] != 199011 {
		t.Fatalf("without the tip it watches from the opening height: %v", f.starts)
	}
}

// A save keeps the height of every account the wallet holds no scanner for at the moment. While
// it is locked it holds none, and as it unlocks it holds only some: a save then wrote those only
// (or nothing), and every other account scanned again from its birth at the next start.
func TestASaveKeepsTheAccountsNotOpenYet(t *testing.T) {
	file := filepath.Join(t.TempDir(), "checkpoints.json")
	s := &service{args: ServiceArgs{CheckpointFile: file}, lock: &sync.RWMutex{},
		checkpoints: map[string]uint32{"a": 209000, "b": 208500}, scanners: map[string]*scannerService{}}
	s.saveCheckpoints() // locked: no scanner yet
	if got := loadCheckpoints(file); got["a"] != 209000 || got["b"] != 208500 || len(got) != 2 {
		t.Fatalf("a save while locked keeps every saved height: %v", got)
	}
	s.scanners["a"] = quietScanner(&fakeSvc{low: 209100}, 93800, 0) // "a" opened, "b" not yet
	s.saveCheckpoints()
	if got := loadCheckpoints(file); got["a"] != 209100 || got["b"] != 208500 || len(got) != 2 {
		t.Fatalf("an opened account's height is its scanner's, the other's is kept: %v", got)
	}
}
