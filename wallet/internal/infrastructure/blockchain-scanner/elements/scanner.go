package elements_scanner

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/aejkcs50/seqdex/wallet/internal/core/domain"
	"github.com/aejkcs50/seqdex/wallet/internal/infrastructure/blockchain-scanner/scanner"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	log "github.com/sirupsen/logrus"
	"github.com/vulpemventures/go-elements/confidential"
	"github.com/vulpemventures/go-elements/elementsutil"
	"github.com/vulpemventures/go-elements/transaction"
	"github.com/vulpemventures/neutrino-elements/pkg/blockservice"
	"github.com/vulpemventures/neutrino-elements/pkg/repository"
)

// unspentWatchItem is a scanner.WatchItem that matches an output by its exact
// output script. It replaces scanner.NewUnspentWatchItemFromAddress, whose
// internal address.ToOutputScript hardcodes the Liquid network HRPs and so
// silently returns a nil item for Sequentia addresses (causing a nil-pointer
// panic in the scanner). We build the item directly from the wallet-derived
// output script, which is HRP-agnostic.
type unspentWatchItem struct {
	outputScript []byte
}

func (u *unspentWatchItem) Bytes() []byte { return u.outputScript }

func (u *unspentWatchItem) Match(tx *transaction.Transaction) bool {
	for _, out := range tx.Outputs {
		if bytes.Equal(u.outputScript, out.Script) {
			return true
		}
	}
	return false
}

func (u *unspentWatchItem) EventType() scanner.EventType {
	return scanner.UnspentUtxo
}

type scannerService struct {
	accountName         string
	svc                 scanner.Service
	blindingKeys        map[string][]byte
	startingBlockHeight uint32
	// resumeFrom is where every watch starts after a restart: the account's saved height
	// less the margin (or its birth when it has none)
	resumeFrom uint32
	// resumeHash is the block below resumeFrom as the last run scanned it (nil: not kept): the
	// watches resuming there carry it, so a reorg made while the wallet was stopped is found
	resumeHash *chainhash.Hash
	// pending holds the block height of every coin report handed to the account's listener and
	// not yet stored by it, oldest first: a saved height never passes the oldest of them
	pmu     sync.Mutex
	pending []uint32
	chTxs               chan *domain.Transaction
	chUtxos             chan []*domain.Utxo
	lock                *sync.RWMutex
	// done ends the report loop; reports waits for it to have ended, so the channels it
	// sends on are closed only once nothing can send on them.
	done    chan struct{}
	reports *sync.WaitGroup

	log  func(format string, a ...interface{})
	warn func(err error, format string, a ...interface{})
}

func newScannerSvc(
	accountName string,
	startingBlockHeight uint32,
	filtersDb repository.FilterRepository,
	headersDb repository.BlockHeaderRepository,
	blockSvc blockservice.BlockService, genesisHash *chainhash.Hash,
) *scannerService {
	logFn := func(format string, a ...interface{}) {
		format = fmt.Sprintf("scanner: %s", format)
		log.Debugf(format, a...)
	}
	warnFn := func(err error, format string, a ...interface{}) {
		format = fmt.Sprintf("scanner: %s", format)
		log.WithError(err).Warnf(format, a...)
	}
	scannerSvc := &scannerService{
		accountName:         accountName,
		svc:                 scanner.New(filtersDb, headersDb, blockSvc, genesisHash),
		blindingKeys:        make(map[string][]byte),
		startingBlockHeight: startingBlockHeight,
		chTxs:               make(chan *domain.Transaction, 10),
		chUtxos:             make(chan []*domain.Utxo, 10),
		lock:                &sync.RWMutex{},
		done:                make(chan struct{}),
		reports:             &sync.WaitGroup{},
		log:                 logFn,
		warn:                warnFn,
	}
	if n, ok := scannerSvc.svc.(interface{ SetName(string) }); ok {
		n.SetName(accountName)
	}
	scannerSvc.setFloor(startingBlockHeight)
	chReports, _ := scannerSvc.svc.Start()
	scannerSvc.reports.Add(1)
	go func() {
		defer scannerSvc.reports.Done()
		scannerSvc.listenToReports(chReports)
	}()
	return scannerSvc
}

func (s *scannerService) stop() {
	close(s.done)
	s.svc.Stop()
	s.reports.Wait()
	close(s.chTxs)
	close(s.chUtxos)
}

// deliver hands the account's listener what a report found, waiting for it to take it.
// A report must never be dropped: a dropped one is a coin the wallet never learns it
// received, or a coin it keeps offering after it was spent. It gives up only when the
// scanner stops.
func (s *scannerService) deliverUtxos(utxos []*domain.Utxo) bool {
	select {
	case s.chUtxos <- utxos:
		return true
	case <-s.done:
		return false
	}
}

func (s *scannerService) deliverTx(tx *domain.Transaction) bool {
	select {
	case s.chTxs <- tx:
		return true
	case <-s.done:
		return false
	}
}

// startFor is where a watch starts: the height the account had been scanned to when the wallet
// started (less the margin), for every watch it sets. An address or coin the account held then was
// watched up to there by the last run; one it gains later has no history before the start. A
// watch set after the opening ones started from the account's birth before, and an account whose
// addresses were watched after the wallet was unlocked scanned 100,000 blocks again on every
// restart.
// adoptBirthday sets the account's birthday, and where its watches resume, on a scanner made
// without one; true when it did.
func (s *scannerService) adoptBirthday(birth, resume uint32, resumeHash *chainhash.Hash) bool {
	s.lock.Lock()
	defer s.lock.Unlock()
	if s.startingBlockHeight != 0 {
		return false
	}
	s.startingBlockHeight, s.resumeFrom, s.resumeHash = birth, resume, resumeHash
	s.setFloor(birth)
	return true
}

// setFloor tells the scanner the account's birthday: where a scan starts again when a reorg's
// fork point is unknown to the node.
func (s *scannerService) setFloor(h uint32) {
	if f, ok := s.svc.(interface{ SetFloor(uint32) }); ok {
		f.SetFloor(h)
	}
}

// resumeLink is the hash a watch starting at `start` carries: the kept block below the resumed
// height, for a watch that starts exactly there.
func (s *scannerService) resumeLink(start uint32) *chainhash.Hash {
	s.lock.RLock()
	defer s.lock.RUnlock()
	if s.resumeHash != nil && start == s.resumeFrom && s.resumeFrom >= s.startingBlockHeight {
		return s.resumeHash
	}
	return nil
}

func (s *scannerService) pendingPush(h uint32) {
	s.pmu.Lock()
	s.pending = append(s.pending, h)
	s.pmu.Unlock()
}

// stored is the listener's word that it stored the oldest coin report it was handed.
func (s *scannerService) stored() {
	s.pmu.Lock()
	if len(s.pending) > 0 {
		s.pending = s.pending[1:]
	}
	s.pmu.Unlock()
}

// pendingLow is the lowest block height of a coin report not yet stored.
func (s *scannerService) pendingLow() (uint32, bool) {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	if len(s.pending) == 0 {
		return 0, false
	}
	low := s.pending[0]
	for _, h := range s.pending {
		if h < low {
			low = h
		}
	}
	return low, true
}

func (s *scannerService) startFor() uint32 {
	s.lock.RLock()
	defer s.lock.RUnlock()
	if s.resumeFrom > s.startingBlockHeight {
		return s.resumeFrom
	}
	return s.startingBlockHeight
}

// watchAddresses watches addresses the account held when the wallet opened, from where the last
// run had scanned it to (startFor).
func (s *scannerService) watchAddresses(addressesInfo []domain.AddressInfo) {
	s.watchAddressesFrom(addressesInfo, s.startFor())
}

// watchAddressesFrom watches each address not watched yet from block `start`.
func (s *scannerService) watchAddressesFrom(addressesInfo []domain.AddressInfo, start uint32) {
	link := s.resumeLink(start)
	s.lock.Lock()
	defer s.lock.Unlock()

	for _, info := range addressesInfo {
		// Prevent duplicates
		if _, ok := s.blindingKeys[info.Script]; ok {
			continue
		}

		s.blindingKeys[info.Script] = info.BlindingKey
		script, err := hex.DecodeString(info.Script)
		if err != nil || len(script) == 0 {
			s.warn(err, "skip watching address with invalid script %q", info.Script)
			continue
		}
		opts := []scanner.ScanRequestOption{
			scanner.WithWatchItem(&unspentWatchItem{outputScript: script}),
			scanner.WithStartBlock(start),
			scanner.WithPersistentWatch(),
		}
		if link != nil {
			opts = append(opts, scanner.WithLastBlockHash(link))
		}
		s.svc.Watch(opts...)
		s.log(
			"start watching address %s for account %s",
			info.DerivationPath, s.accountName,
		)
	}
}

func (s *scannerService) watchUtxos(utxos []domain.UtxoInfo) {
	// the coins known as the wallet opens: spent before the resumed height, the last run saw it
	start := s.startFor()
	link := s.resumeLink(start)
	s.lock.Lock()
	defer s.lock.Unlock()

	for _, u := range utxos {
		hash, _ := elementsutil.TxIDToBytes(u.TxID)
		item, _ := scanner.NewSpentWatchItemFromInput(
			&transaction.TxInput{Hash: hash, Index: u.VOut}, u.Script,
		)
		opts := []scanner.ScanRequestOption{scanner.WithWatchItem(item), scanner.WithStartBlock(start)}
		if link != nil {
			opts = append(opts, scanner.WithLastBlockHash(link))
		}
		s.svc.Watch(opts...)

		s.log("start watching utxo %s for account %s", u, s.accountName)
	}
}

func (s *scannerService) listenToReports(chReports <-chan scanner.Report) {
	s.log("start listening to incoming reports from node")
	for {
		var r scanner.Report
		var ok bool
		select {
		case r, ok = <-chReports:
			if !ok {
				return
			}
		case <-s.done:
			return
		}
		time.Sleep(time.Millisecond)

		if r.Transaction == nil {
			continue
		}

		tx := r.Transaction
		txid, txHex := networkTx(tx)

		s.log("received report for tx %s", txid)

		var blockHash string
		var blockHeight uint64
		if r.BlockHash != nil {
			blockHash = r.BlockHash.String()
			blockHeight = uint64(r.BlockHeight)
		}
		if !s.deliverTx(&domain.Transaction{
			TxID:  txid,
			TxHex: txHex,
			Accounts: map[string]struct{}{
				s.accountName: {},
			},
			BlockHash:   blockHash,
			BlockHeight: blockHeight,
		}) {
			return
		}

		spentUtxos := make([]*domain.Utxo, 0, len(tx.Inputs))
		for _, in := range tx.Inputs {
			spentUtxos = append(spentUtxos, &domain.Utxo{
				UtxoKey: domain.UtxoKey{
					TxID: elementsutil.TxIDFromBytes(in.Hash),
					VOut: in.Index,
				},
				SpentStatus: domain.UtxoStatus{
					Txid:        txid,
					BlockHeight: blockHeight,
					BlockHash:   blockHash,
				},
			})
		}
		if len(spentUtxos) > 0 {
			s.pendingPush(uint32(blockHeight))
			if !s.deliverUtxos(spentUtxos) {
				return
			}
		}

		newUtxos := make([]*domain.Utxo, 0)
		for i, out := range tx.Outputs {
			if len(out.Script) == 0 {
				continue
			}

			script := hex.EncodeToString(out.Script)
			blindingKey, ok := s.getBlindingKey(script)
			if !ok {
				continue
			}

			revealed, err := confidential.UnblindOutputWithKey(out, blindingKey)
			if err != nil {
				s.warn(err, "failed to unblind utxo with given blinding key")
				continue
			}

			var assetCommitment, valueCommitment []byte
			if out.IsConfidential() {
				valueCommitment, assetCommitment = out.Value, out.Asset
			}

			newUtxos = append(newUtxos, &domain.Utxo{
				UtxoKey: domain.UtxoKey{
					TxID: txid,
					VOut: uint32(i),
				},
				Value:           revealed.Value,
				Asset:           assetFromBytes(revealed.Asset),
				ValueCommitment: valueCommitment,
				AssetCommitment: assetCommitment,
				ValueBlinder:    revealed.ValueBlindingFactor,
				AssetBlinder:    revealed.AssetBlindingFactor,
				Script:          out.Script,
				Nonce:           out.Nonce,
				RangeProof:      out.RangeProof,
				SurjectionProof: out.SurjectionProof,
				AccountName:     s.accountName,
				ConfirmedStatus: domain.UtxoStatus{
					BlockHeight: blockHeight,
					BlockHash:   blockHash,
				},
			})
		}

		if len(newUtxos) > 0 {
			s.pendingPush(uint32(blockHeight))
			if !s.deliverUtxos(newUtxos) {
				return
			}
		}
	}
}

func (s *scannerService) getBlindingKey(script string) ([]byte, bool) {
	s.lock.RLock()
	defer s.lock.RUnlock()

	key, ok := s.blindingKeys[script]
	return key, ok
}

func assetFromBytes(buf []byte) string {
	return hex.EncodeToString(elementsutil.ReverseBytes(buf))
}
