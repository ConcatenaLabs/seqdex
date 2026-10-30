package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	log "github.com/sirupsen/logrus"
	"github.com/aejkcs50/seqdex/wallet/internal/core/domain"
	"github.com/aejkcs50/seqdex/wallet/internal/core/ports"
)

// Notification service has the very simple task of making the event channels
// of the used domain.TransactionRepository and domain.UtxoRepository
// accessible by external clients so that they can get real-time updates on the
// status of the internal wallet.
//
// Each client that asks for the events (a notification stream) gets every event
// on a channel of its own, from the moment it asks until its context ends. An
// event is never held for a client that is not there: publishing used to start
// a goroutine per event that waited on one shared channel until some client
// read it, so a wallet with no stream open (the usual case) kept every event of
// every coin it ever saw, and the goroutine that held it, for as long as it ran.
type NotificationService struct {
	repoManager ports.RepoManager
	bcScanner   ports.BlockchainScanner

	mu       sync.Mutex
	utxoSubs map[chan domain.UtxoEvent]struct{}
	txSubs   map[chan domain.TransactionEvent]struct{}
	dropped  uint64

	log func(format string, a ...interface{})
}

// notifyBuffer is how many events a client may fall behind before the ones past
// that are dropped for it (and said in the log): a client that stops reading
// never holds up the wallet.
const notifyBuffer = 1024

func NewNotificationService(
	repoManager ports.RepoManager, bcScanner ports.BlockchainScanner,
) *NotificationService {
	logFn := func(format string, a ...interface{}) {
		format = fmt.Sprintf("notification service: %s", format)
		log.Debugf(format, a...)
	}
	svc := &NotificationService{
		repoManager: repoManager, bcScanner: bcScanner,
		utxoSubs: map[chan domain.UtxoEvent]struct{}{},
		txSubs:   map[chan domain.TransactionEvent]struct{}{},
		log:      logFn,
	}
	svc.registerHandlersForExternalScripts()
	go svc.listenToInternalTxs()
	go svc.listenToInternalUtxos()

	return svc
}

// GetTxChannel is a channel of the transaction events from now until ctx ends.
func (ns *NotificationService) GetTxChannel(
	ctx context.Context,
) (chan domain.TransactionEvent, error) {
	ch := make(chan domain.TransactionEvent, notifyBuffer)
	ns.mu.Lock()
	ns.txSubs[ch] = struct{}{}
	ns.mu.Unlock()
	go func() {
		<-ctx.Done()
		ns.mu.Lock()
		delete(ns.txSubs, ch)
		ns.mu.Unlock()
	}()
	return ch, nil
}

// GetUtxoChannel is a channel of the coin events from now until ctx ends.
func (ns *NotificationService) GetUtxoChannel(
	ctx context.Context,
) (chan domain.UtxoEvent, error) {
	ch := make(chan domain.UtxoEvent, notifyBuffer)
	ns.mu.Lock()
	ns.utxoSubs[ch] = struct{}{}
	ns.mu.Unlock()
	go func() {
		<-ctx.Done()
		ns.mu.Lock()
		delete(ns.utxoSubs, ch)
		ns.mu.Unlock()
	}()
	return ch, nil
}

func (ns *NotificationService) WatchScript(
	ctx context.Context, scriptHex, blindingKey string,
) (string, error) {
	script, err := hex.DecodeString(scriptHex)
	if err != nil {
		return "", fmt.Errorf("invalid script: must be in hex format")
	}
	var key []byte
	if len(blindingKey) > 0 {
		buf, err := hex.DecodeString(blindingKey)
		if err != nil {
			return "", err
		}
		copy(key, buf)
	}

	buf := sha256.Sum256(script)
	hash, _ := chainhash.NewHash(buf[:])
	label := hash.String()
	externalScript := domain.AddressInfo{
		Account:     label,
		Script:      scriptHex,
		BlindingKey: key,
	}

	done, err := ns.repoManager.ExternalScriptRepository().AddScript(ctx, externalScript)
	if err != nil {
		return "", err
	}
	if done {
		ns.log("added external script with label %s", label)
	}

	return label, nil
}

func (ns *NotificationService) StopWatchingScript(
	ctx context.Context, label string,
) error {
	done, err := ns.repoManager.ExternalScriptRepository().DeleteScript(ctx, label)
	if err != nil {
		return err
	}
	if done {
		ns.log("removed script with label %s", label)
	}
	return nil
}

func (ns *NotificationService) listenToInternalTxs() {
	chTxs := ns.repoManager.TransactionRepository().GetEventChannel()
	for event := range chTxs {
		ns.publishTx(event)
	}
}

func (ns *NotificationService) listenToInternalUtxos() {
	chUtxos := ns.repoManager.UtxoRepository().GetEventChannel()
	for event := range chUtxos {
		ns.publishUtxo(event)
	}
}

// publishUtxo hands the event to every client listening, never waiting on one.
func (ns *NotificationService) publishUtxo(event domain.UtxoEvent) {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	for ch := range ns.utxoSubs {
		select {
		case ch <- event:
		default:
			ns.drop("coin")
		}
	}
}

// publishTx hands the event to every client listening, never waiting on one.
func (ns *NotificationService) publishTx(event domain.TransactionEvent) {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	for ch := range ns.txSubs {
		select {
		case ch <- event:
		default:
			ns.drop("transaction")
		}
	}
}

// drop counts an event a client fell too far behind to take; called with mu held.
func (ns *NotificationService) drop(what string) {
	ns.dropped++
	if ns.dropped&(ns.dropped-1) == 0 { // the 1st, 2nd, 4th, 8th…
		log.Warnf("notification service: a client %d events behind: %d events dropped for it so far (the last a %s event)", notifyBuffer, ns.dropped, what)
	}
}

func (ns *NotificationService) registerHandlersForExternalScripts() {
	// Start watching external scripts as soon as they are persisted.
	ns.repoManager.RegisterHandlerForExternalScriptEvent(
		domain.ExternalScriptAdded, func(event domain.ExternalScriptEvent) {
			ns.bcScanner.WatchForAccount(event.Info.Account, 0, []domain.AddressInfo{event.Info})
			chUtxos := ns.bcScanner.GetUtxoChannel(event.Info.Account)
			chTxs := ns.bcScanner.GetTxChannel(event.Info.Account)
			go ns.listenToUtxoChannel(event.Info.Account, chUtxos)
			go ns.listenToTxChannel(event.Info.Account, chTxs)
		},
	)

	// Stop watching external scripts as soon as they are removed.
	ns.repoManager.RegisterHandlerForExternalScriptEvent(
		domain.ExternalScriptDeleted, func(event domain.ExternalScriptEvent) {
			ns.bcScanner.StopWatchForAccount(event.Info.Account)
		},
	)

	// Start watching all registered external scripts at startup.
	scripts, _ := ns.repoManager.ExternalScriptRepository().GetAllScripts(
		context.Background(),
	)
	for _, script := range scripts {
		ns.bcScanner.WatchForAccount(script.Account, 0, []domain.AddressInfo{script})
		chUtxos := ns.bcScanner.GetUtxoChannel(script.Account)
		chTxs := ns.bcScanner.GetTxChannel(script.Account)
		go ns.listenToUtxoChannel(script.Account, chUtxos)
		go ns.listenToTxChannel(script.Account, chTxs)
	}
}

func (ns *NotificationService) listenToUtxoChannel(
	scriptHash string, chUtxos chan []*domain.Utxo,
) {
	ns.log("start listening to utxo channel for script %s", scriptHash)

	for utxos := range chUtxos {
		var eventType domain.UtxoEventType
		switch {
		case utxos[0].IsConfirmedSpent():
			eventType = domain.UtxoConfirmedSpend
		case utxos[0].IsSpent():
			eventType = domain.UtxoSpent
		case utxos[0].IsConfirmed():
			eventType = domain.UtxoConfirmed
		default:
			eventType = domain.UtxoAdded
		}

		utxoInfo := make([]domain.UtxoInfo, 0, len(utxos))
		for _, u := range utxos {
			utxoInfo = append(utxoInfo, u.Info())
		}
		ns.publishUtxo(domain.UtxoEvent{
			EventType: eventType,
			Utxos:     utxoInfo,
		})
	}
}

func (ns *NotificationService) listenToTxChannel(
	scriptHash string, chTxs chan *domain.Transaction,
) {
	ns.log("start listening to tx channel for script %s", scriptHash)

	for tx := range chTxs {
		eventType := domain.TransactionConfirmed
		if !tx.IsConfirmed() {
			eventType = domain.TransactionAdded
		}
		ns.publishTx(domain.TransactionEvent{
			EventType:   eventType,
			Transaction: tx,
		})
	}
}
