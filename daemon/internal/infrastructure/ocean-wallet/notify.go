package oceanwallet

import (
	"context"
	"fmt"
	"sync"

	log "github.com/sirupsen/logrus"
	pb "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/ocean/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/core/ports"
	"google.golang.org/grpc"
)

type notify struct {
	client              pb.NotificationServiceClient
	chTxNotifications   chan ports.WalletTxNotification
	chUtxoNotifications chan ports.WalletUtxoNotification
	// closed is closed by the service's Close: the streams are not opened again after it
	closed    chan struct{}
	closeOnce sync.Once
}

func newNotify(conn *grpc.ClientConn) (*notify, error) {
	svc := &notify{
		client:              pb.NewNotificationServiceClient(conn),
		chTxNotifications:   make(chan ports.WalletTxNotification),
		chUtxoNotifications: make(chan ports.WalletUtxoNotification),
		closed:              make(chan struct{}),
	}

	txStream, err := svc.client.TransactionNotifications(
		context.Background(), &pb.TransactionNotificationsRequest{},
	)
	if err != nil {
		return nil, fmt.Errorf(
			"failed to open stream for tx notifications: %s", err,
		)
	}
	go svc.startListeningForTxNotifications(txStream)

	utxoStream, err := svc.client.UtxosNotifications(
		context.Background(), &pb.UtxosNotificationsRequest{},
	)
	if err != nil {
		return nil, fmt.Errorf(
			"failed to open stream for utxo notifications: %s", err,
		)
	}
	go svc.startListeningForUtxoNotifications(utxoStream)

	return svc, nil
}

func (m *notify) GetTxNotifications() chan ports.WalletTxNotification {
	return m.chTxNotifications
}

func (m *notify) GetUtxoNotifications() chan ports.WalletUtxoNotification {
	return m.chUtxoNotifications
}

// giveUp ends the process when the wallet stays away longer than walletWait, as a broken
// stream always did before.
var giveUp = func(err error, what string) {
	log.WithError(err).Fatalf("notification handler: error while listenting to %s notifications", what)
}

// startListeningForTxNotifications hands every tx notification on. When the stream breaks or
// ends (the wallet daemon stopped or restarted) it is opened again as soon as the wallet answers, for up
// to walletWait; only then is the process ended. Notifications sent while it was broken are
// not seen, as they were not when the process ended and started again.
func (m *notify) startListeningForTxNotifications(
	stream pb.NotificationService_TransactionNotificationsClient,
) {
	for {
		var err error
		for {
			var n *pb.TransactionNotificationsResponse
			if n, err = stream.Recv(); err != nil {
				break
			}
			select {
			case m.chTxNotifications <- txNotifyInfo{n}:
			default:
			}
		}
		select {
		case <-m.closed:
			return
		default:
		}
		log.WithError(err).Warn("notification handler: the tx notifications stream broke, opening it again")
		if err = waitForWallet(m.closed, func(ctx context.Context) error {
			s, err := m.client.TransactionNotifications(context.Background(), &pb.TransactionNotificationsRequest{})
			if err == nil {
				stream = s
			}
			return err
		}); err != nil {
			if err != errClosed {
				giveUp(err, "tx")
			}
			return
		}
		log.Info("notification handler: the tx notifications stream is open again")
	}
}

// startListeningForUtxoNotifications is startListeningForTxNotifications for utxo
// notifications.
func (m *notify) startListeningForUtxoNotifications(
	stream pb.NotificationService_UtxosNotificationsClient,
) {
	for {
		var err error
		for {
			var n *pb.UtxosNotificationsResponse
			if n, err = stream.Recv(); err != nil {
				break
			}
			select {
			case m.chUtxoNotifications <- utxoNotifyInfo{n}:
			default:
			}
		}
		select {
		case <-m.closed:
			return
		default:
		}
		log.WithError(err).Warn("notification handler: the utxo notifications stream broke, opening it again")
		if err = waitForWallet(m.closed, func(ctx context.Context) error {
			s, err := m.client.UtxosNotifications(context.Background(), &pb.UtxosNotificationsRequest{})
			if err == nil {
				stream = s
			}
			return err
		}); err != nil {
			if err != errClosed {
				giveUp(err, "utxo")
			}
			return
		}
		log.Info("notification handler: the utxo notifications stream is open again")
	}
}

type txNotifyInfo struct {
	*pb.TransactionNotificationsResponse
}

func (i txNotifyInfo) GetEventType() ports.WalletTxEventType {
	return txEventType(i.TransactionNotificationsResponse.GetEventType())
}
func (i txNotifyInfo) GetTxHex() string {
	return i.TransactionNotificationsResponse.GetTxhex()
}
func (i txNotifyInfo) GetBlockDetails() ports.BlockInfo {
	return i.TransactionNotificationsResponse.GetBlockDetails()
}

type txEventType pb.TxEventType

func (t txEventType) IsUnconfirmed() bool {
	return int(t) == int(pb.TxEventType_TX_EVENT_TYPE_UNCONFIRMED)
}
func (t txEventType) IsConfirmed() bool {
	return int(t) == int(pb.TxEventType_TX_EVENT_TYPE_CONFIRMED)
}
func (t txEventType) IsBroadcasted() bool {
	return int(t) == int(pb.TxEventType_TX_EVENT_TYPE_BROADCASTED)
}

type utxoNotifyInfo struct {
	*pb.UtxosNotificationsResponse
}

func (i utxoNotifyInfo) GetEventType() ports.WalletUtxoEventType {
	return utxoEventType(i.UtxosNotificationsResponse.GetEventType())
}
func (i utxoNotifyInfo) GetUtxos() []ports.Utxo {
	utxos := make([]ports.Utxo, 0, len(i.UtxosNotificationsResponse.GetUtxos()))
	for _, u := range i.UtxosNotificationsResponse.GetUtxos() {
		utxos = append(utxos, utxoInfo{u})
	}
	return utxos
}
func (i utxoNotifyInfo) GetBlockDetails() ports.BlockInfo {
	// utxo := i.UtxosNotificationsResponse.GetUtxos()[0]
	// if status := utxo.GetSpentStatus(); status != nil {
	// 	return status.GetBlockInfo()
	// }

	// return utxo.GetConfirmedStatus().GetBlockInfo()
	return nil
}

type utxoEventType pb.UtxoEventType

func (t utxoEventType) IsUnconfirmed() bool {
	return int(t) == int(pb.UtxoEventType_UTXO_EVENT_TYPE_NEW)
}
func (t utxoEventType) IsSpent() bool {
	return int(t) == int(pb.UtxoEventType_UTXO_EVENT_TYPE_SPENT)
}
func (t utxoEventType) IsConfirmed() bool {
	return int(t) == int(pb.UtxoEventType_UTXO_EVENT_TYPE_CONFIRMED)
}
func (t utxoEventType) IsLocked() bool {
	return int(t) == int(pb.UtxoEventType_UTXO_EVENT_TYPE_LOCKED)
}
func (t utxoEventType) IsUnlocked() bool {
	return int(t) == int(pb.UtxoEventType_UTXO_EVENT_TYPE_UNLOCKED)
}

type utxoInfo struct {
	*pb.Utxo
}

func (i utxoInfo) GetConfirmedStatus() ports.UtxoStatus {
	return utxoStatusInfo{i.Utxo.GetConfirmedStatus()}
}

func (i utxoInfo) GetSpentStatus() ports.UtxoStatus {
	return utxoStatusInfo{i.Utxo.GetSpentStatus()}
}

type utxoStatusInfo struct {
	*pb.UtxoStatus
}

func (i utxoStatusInfo) GetBlockInfo() ports.BlockInfo {
	return i.UtxoStatus.GetBlockInfo()
}
