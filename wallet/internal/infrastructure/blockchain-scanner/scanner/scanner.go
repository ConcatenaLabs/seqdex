package scanner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/vulpemventures/go-elements/descriptor"

	"github.com/btcsuite/btcd/btcutil/gcs/builder"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/sirupsen/logrus"
	"github.com/vulpemventures/go-elements/block"
	"github.com/vulpemventures/go-elements/transaction"
	"github.com/vulpemventures/neutrino-elements/pkg/blockservice"
	"github.com/vulpemventures/neutrino-elements/pkg/repository"

	log "github.com/sirupsen/logrus"
)

const (
	UnspentUtxo EventType = iota
	SpentUtxo

	numOsScripts = 100
)

type EventType int

type Report struct {
	// Transaction is the transaction that includes the item that was found.
	Transaction *transaction.Transaction

	// BlockHash is the block hash of the block that includes the transaction.
	BlockHash   *chainhash.Hash
	BlockHeight uint32

	// the request resolved by the report
	Request *ScanRequest
}

type Service interface {
	// Start runs a go-routine in order to handle incoming requests via Watch
	Start() (<-chan Report, error)
	// Stop the scanner
	Stop()
	// Watch add a new request to the queue
	Watch(...ScanRequestOption)
	// WatchDescriptorWallet imports wallet descriptor, generates scripts and which
	//for specific events for those scripts
	WatchDescriptorWallet(
		requestID uuid.UUID,
		descriptor string,
		eventType []EventType,
		blockStart int,
	) error
}

type scannerService struct {
	// inFlight is the first height the pass running now has not scanned yet, +1 (0: none): with
	// the queue's lowest start it bounds what every request has scanned (LowestUnscanned). It
	// moves with the pass, block by block, so a checkpoint taken during a long pass (a first
	// scan from an account's birth) keeps what the pass has done, and a restart resumes there
	// rather than at the pass's start.
	inFlight      atomic.Uint32
	// inFlightAt is the pass's first unscanned height with the hash of the block below it, set
	// together (LowestUnscannedAt)
	inFlightAt atomic.Pointer[heightHash]
	// floor is where a scan starts again when a reorg's fork point cannot be found (the node no
	// longer knows the orphaned branch): the account's birthday (SetFloor)
	floor atomic.Uint32
	// name says whose scanner this is in its log lines (SetName); progressAt is when it last
	// said how far a pass had got
	name          string
	progressAt    time.Time
	started       bool
	requestsQueue *scanRequestQueue
	filterDB      repository.FilterRepository
	headerDB      repository.BlockHeaderRepository
	genesisHash   *chainhash.Hash
	blockService  blockservice.BlockService
	quitCh        chan struct{}
}

var _ Service = (*scannerService)(nil)

type heightHash struct {
	height uint32
	hash   *chainhash.Hash
}

// SetFloor sets the lowest height this scanner's account can have history at (its birthday).
func (s *scannerService) SetFloor(h uint32) { s.floor.Store(h) }

// LowestUnscannedAt is LowestUnscanned with the hash of the block just below that height, as the
// scan last saw it (nil when it has none): saved with the height, it lets a restart check that
// block against the node and find a reorg that happened while the wallet was stopped.
func (s *scannerService) LowestUnscannedAt() (uint32, *chainhash.Hash, bool) {
	low, ok := uint32(0), false
	var hash *chainhash.Hash
	if r := s.requestsQueue.peek(); r != nil {
		low, hash, ok = r.StartHeight, r.LastBlockHash, true
	}
	if f := s.inFlightAt.Load(); f != nil && (!ok || f.height < low) {
		low, hash, ok = f.height, f.hash, true
	}
	return low, hash, ok
}

// SetName names the scanner in its log lines (the account it scans for).
func (s *scannerService) SetName(name string) { s.name = name }

// progressEvery is how often a pass says how far it has got.
const progressEvery = time.Minute

func New(
	filterDB repository.FilterRepository,
	headerDB repository.BlockHeaderRepository,
	blockSvc blockservice.BlockService,
	genesisHash *chainhash.Hash,
) Service {
	return &scannerService{
		requestsQueue: newScanRequestQueue(),
		filterDB:      filterDB,
		headerDB:      headerDB,
		blockService:  blockSvc,
		quitCh:        make(chan struct{}),
		genesisHash:   genesisHash,
	}
}

func (s *scannerService) Start() (<-chan Report, error) {
	log.Debugln("scanner: starting scanner ...")

	if s.started {
		return nil, fmt.Errorf("scanner already started")
	}

	s.quitCh = make(chan struct{}, 1)
	resultCh := make(chan Report)
	// start the requests manager
	go s.requestsManager(resultCh)

	s.started = true
	return resultCh, nil
}

func (s *scannerService) Stop() {
	log.Debugln("scanner: stopping scanner ...")

	s.quitCh <- struct{}{}
	s.started = false
	//s.requestsQueue = newScanRequestQueue() TODO: commented cause data race
}

// LowestUnscanned is the lowest block height some watch has not scanned yet: every block
// below it is scanned for every watch the scanner holds. ok is false while it holds none.
func (s *scannerService) LowestUnscanned() (uint32, bool) {
	low, ok := uint32(0), false
	if r := s.requestsQueue.peek(); r != nil {
		low, ok = r.StartHeight, true
	}
	if f := s.inFlight.Load(); f > 0 && (!ok || f-1 < low) {
		low, ok = f-1, true
	}
	return low, ok
}

func (s *scannerService) Watch(opts ...ScanRequestOption) {
	req := newScanRequest(opts...)
	s.requestsQueue.enqueue(req)
}

func (s *scannerService) WatchDescriptorWallet(
	requestID uuid.UUID,
	desc string,
	eventType []EventType,
	blockStart int,
) error {
	for _, v := range eventType {
		switch v {
		case UnspentUtxo:
			wallet, err := descriptor.Parse(desc)
			if err != nil {
				return err
			}

			var scripts []descriptor.ScriptResponse

			if wallet.IsRange() {
				scripts, err = wallet.Script(descriptor.WithRange(numOsScripts))
				if err != nil {
					return err
				}

				for _, v := range scripts {
					s.Watch(
						WithRequestID(requestID),
						WithStartBlock(uint32(blockStart)),
						WithWatchItem(&UnspentWatchItem{
							outputScript: v.Script,
						}),
						WithPersistentWatch(),
					)
				}
			} else {
				scripts, err = wallet.Script(nil)
				if err != nil {
					return err
				}

				s.Watch(
					WithRequestID(requestID),
					WithStartBlock(uint32(blockStart)),
					WithWatchItem(&UnspentWatchItem{
						outputScript: scripts[0].Script,
					}),
					WithPersistentWatch(),
				)
			}

		}
	}

	return nil
}

// requestsManager is responsible to resolve the requests that are waiting for in the queue.
func (s *scannerService) requestsManager(ch chan<- Report) {
	defer close(s.quitCh)

	for {
		s.requestsQueue.cond.L.Lock()
		for s.requestsQueue.isEmpty() {
			logrus.Debug("scanner: scanner queue is empty, waiting for new requests")
			s.requestsQueue.cond.Wait() // wait for new requests

			// check if we should quit the routine
			select {
			case <-s.quitCh:
				s.requestsQueue.cond.L.Unlock()
				return
			default:
			}
		}
		s.requestsQueue.cond.L.Unlock()

		// get the next request without removing it from the queue
		nextRequest := s.requestsQueue.peek()
		err := s.requestWorker(nextRequest, ch)
		if err != nil {
			logrus.Errorf("error while scanning: %v", err)
			// the requests are back in the queue where they stopped: try again
			// after a pause rather than hammer a node that is failing
			time.Sleep(2 * time.Second)
		}

		// check if we should quit the routine
		select {
		case <-s.quitCh:
			return
		default:
			continue
		}

	}
}

// reorgRollbackHorizon bounds the fork-point walk only for a headers repository that cannot ask
// the node whether a block is on its active chain (the production one can: its walk has no bound).
const reorgRollbackHorizon = 1000

// activeChainReader answers from the node itself, never a cache, whether a block is on its active
// chain (the production headers repository does; elements/header_repo.go).
type activeChainReader interface {
	BlockOnActiveChain(ctx context.Context, hash chainhash.Hash) (height uint32, prev *chainhash.Hash, onChain bool, err error)
}

// heightForgetter drops cached height-to-hash answers (shared by every account's scanner).
type heightForgetter interface {
	ForgetHeights()
}

func (s *scannerService) forgetHeights() {
	if f, ok := s.headerDB.(heightForgetter); ok {
		f.ForgetHeights()
	}
}

// resumeAfterReorg is the first height to scan again after the block `last`, scanned at
// nextHeight-1, left the node's chain: the height after the newest ancestor of `last` that is
// on the node's active chain. Whether a block is on it is asked of the node itself: the cached
// height-to-hash answers every scanner shares can still name an orphaned block for a few seconds
// after a reorg, and taking such a block for the fork point would leave the new branch's blocks
// below it unscanned. A reorg also clears those answers for every scanner.
func (s *scannerService) resumeAfterReorg(last *chainhash.Hash, nextHeight uint32) uint32 {
	s.forgetHeights()
	ctx := context.Background()
	h := *last
	if ac, ok := s.headerDB.(activeChainReader); ok {
		// however deep: this chain follows its Bitcoin anchor to any depth. When the node no
		// longer knows the orphaned branch, from the account's birthday.
		floor := s.floor.Load()
		for {
			height, prev, onChain, err := ac.BlockOnActiveChain(ctx, h)
			if err != nil {
				log.Warnf("scanner %s: the fork point below block %s is unknown to the node (%v): scanning again from %d", s.name, h, err, floor)
				return floor
			}
			if onChain {
				if height+1 < floor {
					return floor
				}
				return height + 1
			}
			if prev == nil || height <= floor {
				return floor
			}
			h = *prev
		}
	}
	fallback := uint32(0) // unsigned: a chain shorter than the horizon rolls back to genesis
	if nextHeight > reorgRollbackHorizon {
		fallback = nextHeight - reorgRollbackHorizon
	}
	// a headers repository that cannot say: the walk compares with its height lookup, at most
	// reorgRollbackHorizon blocks back
	for i := 0; i <= reorgRollbackHorizon; i++ {
		hdr, err := s.headerDB.GetBlockHeader(ctx, h)
		if err != nil || hdr == nil {
			return fallback
		}
		onChain, err := s.headerDB.GetBlockHashByHeight(ctx, hdr.Height)
		if err == nil && onChain.IsEqual(&h) {
			return hdr.Height + 1
		}
		if hdr.Height == 0 {
			return 0
		}
		prev, err := chainhash.NewHash(hdr.PrevBlockHash)
		if err != nil {
			return fallback
		}
		h = *prev
	}
	return fallback
}

// catchUp bounds how many scanners read blocks at once while they are more than catchUpLag
// blocks behind the tip (SetCatchUpParallel): every account scans for itself, and after a
// restart dozens of them catching up together took every core the wallet was given and the
// memory of dozens of passes at once. A scanner following the tip never waits for it.
var catchUp = make(chan struct{}, 4)

// catchUpLag is how far behind the tip a pass counts as catching up.
const catchUpLag = 6

// SetCatchUpParallel sets how many scanners may catch up at once (at least 1). Call it before
// any scanner starts.
func SetCatchUpParallel(n int) {
	if n < 1 {
		n = 1
	}
	catchUp = make(chan struct{}, n)
}

// errNotYet stops a pass at a block whose filter or body the node cannot give
// yet: the pass resumes there, so the block is scanned, never skipped.
var errNotYet = errors.New("block not available yet")

// filterLagBlocks is how far below the tip a block's filter may still be on its way: a block
// deeper than that with no filter is scanned in full.
const filterLagBlocks = 6

// requestWorker scans forward from the request's start height for every block
// holding a watched item. Each request it takes off the queue goes back on it,
// whatever happens, at the first height it has not scanned (with the hash of
// the last block it did scan, for the reorg check), so a pass never scans a
// block twice for the same request and an error never drops one.
func (s *scannerService) requestWorker(req *ScanRequest, reportsChan chan<- Report) (err error) {
	nextBatch := make([]*ScanRequest, 0)
	nextHeight := req.StartHeight
	s.inFlight.Store(req.StartHeight + 1)
	defer s.inFlight.Store(0)
	defer s.inFlightAt.Store(nil)
	var lastHash *chainhash.Hash
	defer func() {
		for _, r := range nextBatch {
			if r.StartHeight < nextHeight {
				r.StartHeight = nextHeight
				if lastHash != nil {
					r.LastBlockHash = lastHash
				}
			}
			s.requestsQueue.enqueue(r)
		}
		if errors.Is(err, errNotYet) {
			time.Sleep(2 * time.Second)
			err = nil
		}
	}()

	chainTip, err := s.headerDB.ChainTip(context.Background())
	if err != nil {
		return err
	}

	// SEQUENTIA reorg handling: if this persistent request remembers the hash of the
	// last block it scanned (the block at StartHeight-1), verify that block is still
	// on the node's active chain. A deep parent-chain (Bitcoin) reorg can orphan it
	// and shorten the chain; if so we MUST follow the canonical chain rather than
	// freeze on the orphaned block (anchoring supremacy: a reorg always wins). Roll
	// the scan back to genesis and let the forward scan re-derive on the new branch;
	// the report consumer keys UTXOs by outpoint, so re-derivation is idempotent.
	if req.LastBlockHash != nil && nextHeight > 0 {
		ancHash, herr := s.headerDB.GetBlockHashByHeight(context.Background(), nextHeight-1)
		switch {
		case herr == nil && ancHash.IsEqual(req.LastBlockHash):
			// no reorg at/below our resume point; continue forward as normal, each block
			// linked to the one before it
			lastHash = req.LastBlockHash
		case herr == nil || errors.Is(herr, repository.ErrBlockNotFound):
			// the remembered block was orphaned (hash differs) or the chain shrank past it: a
			// reorg (anchoring supremacy: a Bitcoin reorg always wins, and this chain follows it,
			// to any depth). The scan resumes at the block after the last one the orphaned branch
			// shares with the node's chain (resumeAfterReorg). Re-derivation is idempotent (UTXOs
			// keyed by outpoint).
			rollback := s.resumeAfterReorg(req.LastBlockHash, nextHeight)
			log.Warnf("scanner %s: block %d it last scanned is no longer on the chain (a reorg): scanning again from %d",
				s.name, nextHeight-1, rollback)
			req.StartHeight = rollback
			req.LastBlockHash = nil
			nextHeight = rollback
		default:
			return herr
		}
	}

	// Nothing to scan yet: this request is ahead of the node's live tip (e.g. the
	// chain shrank below it after a reorg). Wait for the tip to advance instead of
	// busy-looping on a request that cannot make progress.
	if nextHeight > chainTip.Height {
		time.Sleep(2 * time.Second)
		return nil
	}

	// a pass is linked from the block below its start: the one the request remembers, or else
	// the one the node names at that height now
	if lastHash == nil && nextHeight > 0 {
		if h, herr := s.headerDB.GetBlockHashByHeight(context.Background(), nextHeight-1); herr == nil {
			lastHash = h
		}
	}
	s.inFlightAt.Store(&heightHash{nextHeight, lastHash})

	slots := catchUp
	held := false
	defer func() {
		if held {
			<-slots
		}
	}()
	for nextHeight <= chainTip.Height {
		// far behind the tip, a block is read only while holding one of the catch-up slots;
		// one is taken per block, so the scanners catching up take turns block by block
		if behind := chainTip.Height-nextHeight > catchUpLag; behind && !held {
			slots <- struct{}{}
			held = true
		}
		if err := s.scanOne(&nextHeight, &lastHash, &nextBatch, chainTip, reportsChan); err != nil {
			if errors.Is(err, errStopPass) {
				return nil
			}
			return err
		}
		if held {
			<-slots
			held = false
		}
		if time.Since(s.progressAt) >= progressEvery {
			s.progressAt = time.Now()
			log.Infof("scanner %s: scanned to block %d of %d, %d to go", s.name, nextHeight-1, chainTip.Height, chainTip.Height-(nextHeight-1))
		}

		chainTip, err = s.headerDB.ChainTip(context.Background())
		if err != nil {
			return err
		}
	}

	return nil
}

// errStopPass ends a pass without an error: the chain shrank below the next height.
var errStopPass = errors.New("the chain ends below the next height")

// scanOne scans the block at *nextHeight for every request waiting at it or carried in the
// batch, and moves *nextHeight and *lastHash past it. errStopPass: the chain ends below it.
func (s *scannerService) scanOne(
	nextHeight *uint32, lastHash **chainhash.Hash, nextBatch *[]*ScanRequest,
	chainTip *block.Header, reportsChan chan<- Report,
) error {
	// append all the requests with start height = nextHeight. One that remembers another block
	// below it than this pass scanned (it was matched in a block a reorg has orphaned) is not
	// carried on: it starts again after its own fork point, in a pass of its own.
	for _, r := range s.requestsQueue.dequeueAtHeight(*nextHeight) {
		if r.LastBlockHash != nil && *lastHash != nil && !r.LastBlockHash.IsEqual(*lastHash) {
			r.StartHeight = s.resumeAfterReorg(r.LastBlockHash, *nextHeight)
			r.LastBlockHash = nil
			s.requestsQueue.enqueue(r)
			continue
		}
		*nextBatch = append(*nextBatch, r)
	}

	itemsBytes := make([][]byte, len(*nextBatch))
	for i, req := range *nextBatch {
		itemsBytes[i] = req.Item.Bytes()
	}

	// get the block hash for height
	var blockHash *chainhash.Hash
	if *nextHeight == 0 {
		blockHash = s.genesisHash
	} else {
		var err error
		blockHash, err = s.headerDB.GetBlockHashByHeight(context.Background(), *nextHeight)
		if err != nil {
			if errors.Is(err, repository.ErrBlockNotFound) {
				// the chain shrank past nextHeight mid-scan (reorg / caught up to the live
				// tip): stop here; the requests are re-enqueued and retried next cycle.
				return errStopPass
			}
			return err
		}
	}

	// each block a pass scans is the child of the one it scanned before: the shared height
	// answers can name an orphaned block for a moment after a reorg, and a pass that went from an
	// orphaned block to the new branch's next one would never scan the new branch's block at the
	// orphaned height. On a break the pass ends here; its requests keep the last block they
	// scanned, so the next pass finds the reorg and scans again from its fork point.
	if *lastHash != nil {
		hdr, err := s.headerDB.GetBlockHeader(context.Background(), *blockHash)
		if err != nil {
			if errors.Is(err, repository.ErrBlockNotFound) {
				return errStopPass
			}
			return err
		}
		if !bytes.Equal(hdr.PrevBlockHash, (*lastHash).CloneBytes()) {
			s.forgetHeights()
			return errStopPass
		}
	}

	// check with filterDB if the block has one of the items
	matched, err := s.blockFilterMatches(itemsBytes, blockHash)
	if errors.Is(err, errNotYet) && *nextHeight+filterLagBlocks <= chainTip.Height {
		// No filter for a block well below the tip: the node will not index it now, so the
		// block itself is read and matched in full rather than waited for, or skipped.
		matched, err = true, nil
	}
	if err != nil {
		return err
	}

	if matched {
		reports, remainReqs, err := s.extractBlockMatches(blockHash, *nextBatch)
		if err != nil {
			return err
		}
		// the matched requests leave the batch here: each persistent one is
		// watched again below from the block after its match
		*nextBatch = remainReqs

		rewatched := make(map[*ScanRequest]bool)
		for _, report := range reports {
			// send the report to the output channel
			reportsChan <- report

			// if the request is persistent, the scanner keeps watching the item from the
			// next block: once, however many of the block's transactions it matched
			if report.Request.IsPersistent && !rewatched[report.Request] {
				rewatched[report.Request] = true
				s.Watch(
					WithRequestID(report.Request.ClientID),
					WithStartBlock(report.BlockHeight+1),
					WithWatchItem(report.Request.Item),
					WithPersistentWatch(),
					WithLastBlockHash(report.BlockHash),
				)
			}
		}
	}

	// the block is scanned for every request in the batch
	*lastHash = blockHash
	*nextHeight++
	s.inFlight.Store(*nextHeight + 1)
	s.inFlightAt.Store(&heightHash{*nextHeight, blockHash})
	return nil
}

func (s *scannerService) blockFilterMatches(items [][]byte, blockHash *chainhash.Hash) (bool, error) {
	filterToFetchKey := repository.FilterKey{
		BlockHash:  blockHash.CloneBytes(),
		FilterType: repository.RegularFilter,
	}

	filter, err := s.filterDB.GetFilter(context.Background(), filterToFetchKey)
	if err != nil {
		if err == repository.ErrFilterNotFound {
			return false, errNotYet
		}
		return false, err
	}

	gcsFilter, err := filter.GcsFilter()
	if err != nil {
		return false, err
	}

	key := builder.DeriveKey(blockHash)
	matched, err := gcsFilter.MatchAny(key, items)
	if err != nil {
		return false, err
	}

	return matched, nil
}

func (s *scannerService) extractBlockMatches(blockHash *chainhash.Hash, requests []*ScanRequest) ([]Report, []*ScanRequest, error) {
	block, err := s.blockService.GetBlock(blockHash)
	if err != nil {
		if err == blockservice.ErrorBlockNotFound {
			return nil, nil, errNotYet // the block matched a filter: never skip it
		}

		return nil, nil, err
	}

	results := make([]Report, 0)

	remainRequests := make([]*ScanRequest, 0)

	for _, req := range requests {
		reqMatchedAtLeastOneTime := false
		for _, tx := range block.TransactionsData.Transactions {
			if req.Item.Match(tx) {
				reqMatchedAtLeastOneTime = true
				results = append(results, Report{
					Transaction: tx,
					BlockHash:   blockHash,
					BlockHeight: block.Header.Height,
					Request:     req,
				})
			}
		}

		if !reqMatchedAtLeastOneTime {
			remainRequests = append(remainRequests, req)
		}
	}

	return results, remainRequests, nil
}
