package elements_scanner

import (
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

// Saved heights are read back by the next run; the watches set as the wallet opens resume there,
// and an address watched later starts from the account's birth.
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
	if len(f.starts) != 2 || f.starts[0] != 190500-checkpointMargin || f.starts[1] != 1000 {
		t.Fatalf("the opening watches resume, a later one starts at birth: %v", f.starts)
	}
}
