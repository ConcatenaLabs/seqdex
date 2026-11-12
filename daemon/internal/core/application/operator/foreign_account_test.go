package operator

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aejkcs50/seqdex/daemon/internal/core/application/pubsub"
	"github.com/aejkcs50/seqdex/daemon/internal/core/application/wallet"
	"github.com/aejkcs50/seqdex/daemon/internal/core/domain"
	"github.com/aejkcs50/seqdex/daemon/internal/core/ports"
	"github.com/aejkcs50/seqdex/daemon/internal/infrastructure/storage/db/inmemory"
	"github.com/aejkcs50/seqdex/daemon/pkg/seqnet"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/vulpemventures/go-elements/elementsutil"
	"github.com/vulpemventures/go-elements/payment"
	"github.com/vulpemventures/go-elements/transaction"
)

const nativeAsset = "5ac9f65c0efcc4775e0baec4ec03abdde22473cd3cf33c0419ca290e0751b225"

// The wallet daemon is shared: besides this daemon's markets it holds the
// makers' own accounts. A wallet with two accounts: "sc-SEQ-USDX" (a maker's,
// no market here) and "mkt" (a market), each with one address.
type sharedWallet struct {
	ports.WalletService
	mbk   string
	addrs map[string]string // account label -> address
	ch    chan ports.WalletTxNotification
}
type sharedWalletAPI struct {
	ports.Wallet
	w *sharedWallet
}
type sharedAccountAPI struct {
	ports.Account
	w *sharedWallet
}
type sharedTxAPI struct{ ports.Transaction }
type sharedNotifAPI struct {
	ports.Notification
	w *sharedWallet
}
type sharedInfo struct {
	ports.WalletInfo
	mbk string
}
type sharedAccount struct {
	ports.WalletAccount
	label, namespace, mbk string
}

func (w *sharedWallet) Wallet() ports.Wallet             { return sharedWalletAPI{w: w} }
func (w *sharedWallet) Account() ports.Account           { return sharedAccountAPI{w: w} }
func (w *sharedWallet) Transaction() ports.Transaction   { return sharedTxAPI{} }
func (w *sharedWallet) Notification() ports.Notification { return sharedNotifAPI{w: w} }
func (a sharedWalletAPI) Info(context.Context) (ports.WalletInfo, error) {
	return sharedInfo{mbk: a.w.mbk}, nil
}
func (i sharedInfo) GetAccounts() []ports.WalletAccount {
	return []ports.WalletAccount{
		sharedAccount{label: "sc-SEQ-USDX", namespace: "bip84-account3", mbk: i.mbk},
		sharedAccount{label: "mkt", namespace: "bip84-account1", mbk: i.mbk},
	}
}
func (sharedInfo) GetNetwork() string                { return "regtest" }
func (sharedInfo) GetNativeAsset() string            { return nativeAsset }
func (a sharedAccount) GetLabel() string             { return a.label }
func (a sharedAccount) GetNamespace() string         { return a.namespace }
func (a sharedAccount) GetMasterBlindingKey() string { return a.mbk }
func (a sharedAccountAPI) ListAddresses(_ context.Context, account string) ([]string, error) {
	return []string{a.w.addrs[account]}, nil
}
func (sharedAccountAPI) GetBalance(context.Context, string) (map[string]ports.Balance, error) {
	return nil, nil
}
func (sharedTxAPI) GetTransaction(context.Context, string) (string, error) { return "", nil }
func (n sharedNotifAPI) GetTxNotifications() chan ports.WalletTxNotification {
	return n.w.ch
}
func (n sharedNotifAPI) GetUtxoNotifications() chan ports.WalletUtxoNotification {
	return make(chan ports.WalletUtxoNotification)
}

type unconfirmed struct{ ports.WalletTxEventType }

func (unconfirmed) IsUnconfirmed() bool { return true }
func (unconfirmed) IsBroadcasted() bool { return false }
func (unconfirmed) IsConfirmed() bool   { return false }

type txNotification struct {
	ports.WalletTxNotification
	accounts []string
	txHex    string
}

func (n txNotification) GetEventType() ports.WalletTxEventType { return unconfirmed{} }
func (n txNotification) GetAccountNames() []string             { return n.accounts }
func (n txNotification) GetTxHex() string                      { return n.txHex }

// Records what the daemon publishes.
type recordingPubSub struct {
	ports.SecurePubSub
	mu        sync.Mutex
	published chan string
}

func (p *recordingPubSub) Publish(topic, message string) error {
	p.published <- message
	return nil
}

func newAddress(t *testing.T) (string, []byte) {
	key, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	net := seqnet.SequentiaRegtest
	p := payment.FromPublicKey(key.PubKey(), &net, nil)
	addr, err := p.WitnessPubKeyHash()
	if err != nil {
		t.Fatal(err)
	}
	return addr, p.WitnessScript
}

// seqdexd crash-looped on the box with a nil dereference at service.go:297:
// every deposit into a wallet account that is not one of its markets (the
// makers' per-pair accounts share the wallet) looked the market up, got none,
// and dereferenced it in a goroutine of its own, which ends the process. A
// deposit into such an account is now skipped for the market event, and the
// deposits of the daemon's own markets are still published.
//
// Run twice: with the accounts' blinding keys (the box's case, which on main
// panics at the market lookup) and with none, as GetInfo answers while the
// wallet is locked (which on main panics earlier, deriving a key from nil).
func TestADepositIntoAnAccountThatIsNotAMarketDoesNotStopTheDaemon(t *testing.T) {
	for _, tc := range []struct{ name, mbk string }{
		{"unlocked wallet", "4cc1f2bba4a1bc1f2e1ab6a9b9e7c2d1f0a9b8c7d6e5f4a3b2c1d0e9f8a7b6c5"},
		{"locked wallet", ""},
	} {
		t.Run(tc.name, func(t *testing.T) { depositIntoAMakersAccount(t, tc.mbk) })
	}
}

func depositIntoAMakersAccount(t *testing.T, mbk string) {
	makerAddr, makerScript := newAddress(t)
	marketAddr, marketScript := newAddress(t)
	w := &sharedWallet{
		mbk:   mbk,
		addrs: map[string]string{"sc-SEQ-USDX": makerAddr, "mkt": marketAddr},
		ch:    make(chan ports.WalletTxNotification),
	}
	walletSvc, err := wallet.NewService(w, "")
	if err != nil {
		t.Fatal(err)
	}

	repo := inmemory.NewRepoManager()
	usdx := "1111111111111111111111111111111111111111111111111111111111111111"
	mkt, err := domain.NewMarket(nativeAsset, usdx, "mkt", 25, 25, 0, 0, 8, 8, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.MarketRepository().AddMarket(context.Background(), mkt); err != nil {
		t.Fatal(err)
	}
	ps := &recordingPubSub{published: make(chan string, 4)}

	accounts := &accountMap{&sync.RWMutex{}, map[string]string{
		"bip84-account3": "sc-SEQ-USDX", "bip84-account1": "mkt",
	}}
	svc := &service{
		wallet: walletSvc, pubsub: pubsub.NewService(ps), repoManager: repo,
		network: walletSvc.Network(), accounts: accounts,
	}

	tx := transaction.NewTx(2)
	tx.AddInput(transaction.NewTxInput(make([]byte, 32), 0))
	asset, _ := elementsutil.AssetHashToBytes(nativeAsset)
	value, _ := elementsutil.ValueToBytes(1000)
	tx.AddOutput(transaction.NewTxOutput(asset, value, makerScript))
	tx.AddOutput(transaction.NewTxOutput(asset, value, marketScript))
	txHex, err := tx.ToHex()
	if err != nil {
		t.Fatal(err)
	}

	// The maker's account comes first: on main its goroutine dereferences a
	// nil market and the test binary dies here.
	svc.classifyAndStoreTx()(txNotification{
		accounts: []string{"bip84-account3", "bip84-account1"},
		txHex:    txHex,
	})

	select {
	case msg := <-ps.published:
		if !contains(msg, `"type":"market"`) || !contains(msg, usdx) {
			t.Fatalf("published %s, not the market's deposit", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the market's deposit was never published")
	}
	select {
	case msg := <-ps.published:
		t.Fatalf("published a second event: %s", msg)
	case <-time.After(200 * time.Millisecond):
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
