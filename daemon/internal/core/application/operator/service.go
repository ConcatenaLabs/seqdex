package operator

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/shopspring/decimal"
	log "github.com/sirupsen/logrus"
	"github.com/aejkcs50/seqdex/daemon/internal/core/application/pubsub"
	"github.com/aejkcs50/seqdex/daemon/internal/core/application/wallet"
	"github.com/aejkcs50/seqdex/daemon/internal/core/domain"
	"github.com/aejkcs50/seqdex/daemon/internal/core/ports"
	"github.com/aejkcs50/seqdex/daemon/pkg/registry"
	"github.com/aejkcs50/seqdex/daemon/pkg/seqnet"
	"github.com/vulpemventures/go-elements/confidential"
	"github.com/vulpemventures/go-elements/elementsutil"
	"github.com/vulpemventures/go-elements/network"
	"github.com/vulpemventures/go-elements/slip77"
	"github.com/vulpemventures/go-elements/transaction"
)

var (
	minSatsPerByte = decimal.NewFromFloat(0.1)
	maxSatsPerByte = decimal.NewFromInt(10000)
)

type service struct {
	wallet      *wallet.Service
	pubsub      *pubsub.Service
	repoManager ports.RepoManager

	feeAccountBalanceThreshold uint64
	network                    network.Network
	accounts                   *accountMap
	milliSatsPerByte           uint64
	// registry sources an asset's precision (decimal places) at market
	// creation. It is nil when no registry URL is configured, in which case
	// precision falls back to the CLI flag / the default.
	registry *registry.Client
}

func NewService(
	walletSvc *wallet.Service, pubsubSvc *pubsub.Service,
	repoManager ports.RepoManager, feeAccountBalanceThreshold uint64,
	satsPerByte decimal.Decimal, registryURL string,
) (*service, error) {
	if walletSvc == nil {
		return nil, fmt.Errorf("missing wallet service")
	}
	if pubsubSvc == nil {
		return nil, fmt.Errorf("missing pubsub service")
	}
	if repoManager == nil {
		return nil, fmt.Errorf("missing repo manager")
	}
	if satsPerByte.LessThan(minSatsPerByte) ||
		satsPerByte.GreaterThan(maxSatsPerByte) {
		return nil, fmt.Errorf(
			"sats per byte ratio must be in range [%s, %s]",
			minSatsPerByte, maxSatsPerByte,
		)
	}
	msatsPerByte := satsPerByte.Mul(decimal.NewFromInt(1000)).BigInt().Uint64()
	info, err := walletSvc.Wallet().Info(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to connect to wallet: %s", err)
	}

	labelsByNamespace := make(map[string]string)
	for _, account := range info.GetAccounts() {
		labelsByNamespace[account.GetNamespace()] = account.GetLabel()
	}
	accounts := &accountMap{&sync.RWMutex{}, labelsByNamespace}

	svc := &service{
		walletSvc, pubsubSvc, repoManager, feeAccountBalanceThreshold,
		walletSvc.Network(), accounts, msatsPerByte,
		registry.NewClient(registryURL),
	}

	svc.wallet.RegisterHandlerForTxEvent(svc.classifyAndStoreTx())
	svc.wallet.RegisterHandlerForTxEvent(svc.checkAccountsLowBalance())
	return svc, nil
}

func (s *service) ListMarkets(ctx context.Context) ([]ports.MarketInfo, error) {
	markets, err := s.repoManager.MarketRepository().GetAllMarkets(ctx)
	if err != nil {
		return nil, err
	}

	list := make([]ports.MarketInfo, 0)
	for _, market := range markets {
		balance, err := s.wallet.Account().GetBalance(ctx, market.Name)
		if err != nil {
			log.WithError(err).Warnf("failed to fetch balance for market %s", market.Name)
		}
		list = append(list, marketInfo{market, balance})
	}
	return list, nil
}

func (s *service) ListTradesForMarket(
	ctx context.Context, market ports.Market, page ports.Page,
) ([]ports.Trade, error) {
	if market == nil {
		return nil, fmt.Errorf("missing market")
	}

	mkt, err := s.repoManager.MarketRepository().GetMarketByAssets(
		ctx, market.GetBaseAsset(), market.GetQuoteAsset(),
	)
	if err != nil {
		return nil, err
	}
	if mkt == nil {
		return nil, fmt.Errorf("market not found")
	}

	trades, err := s.repoManager.TradeRepository().GetAllTradesByMarket(
		ctx, mkt.Name, page,
	)
	if err != nil {
		return nil, err
	}
	return tradeList(trades).toPortableList(), nil
}

func (s *service) ListUtxos(
	ctx context.Context, accountName string, page ports.Page,
) ([]ports.Utxo, []ports.Utxo, error) {
	return s.wallet.Account().ListUtxos(ctx, accountName)
}

func (s *service) ListDeposits(
	ctx context.Context, accountName string, page ports.Page,
) ([]ports.Deposit, error) {
	deposits, err := s.repoManager.DepositRepository().GetDepositsForAccount(
		ctx, accountName, page,
	)
	if err != nil {
		return nil, err
	}
	return depositList(deposits).toPortableList(), nil
}

func (s *service) ListWithdrawals(
	ctx context.Context, accountName string, page ports.Page,
) ([]ports.Withdrawal, error) {
	withdrawals, err := s.repoManager.WithdrawalRepository().GetWithdrawalsForAccount(
		ctx, accountName, page,
	)
	if err != nil {
		return nil, err
	}
	return withdrawalList(withdrawals).toPortableList(), nil
}

func (s *service) checkAccountsLowBalance() func(ports.WalletTxNotification) bool {
	return func(notification ports.WalletTxNotification) bool {
		publishTopic := func(
			account string, balance map[string]ports.Balance, mktInfo *marketInfo,
		) {
			if err := s.pubsub.PublisAccountLowBalanceEvent(
				account, balance, mktInfo,
			); err != nil {
				log.WithError(err).Warnf(
					"pubsub: failed to publish low account balance topic for "+
						"account %s", account,
				)
			} else {
				log.Debugf(
					"pubsub: published low account balance topic for account %s",
					account,
				)
			}
		}

		eventType := notification.GetEventType()
		if eventType.IsUnconfirmed() || eventType.IsBroadcasted() {
			for _, accountNamespace := range notification.GetAccountNames() {
				account := s.accounts.getLabel(accountNamespace)
				if account == domain.FeeFragmenterAccount ||
					account == domain.MarketFragmenterAccount {
					continue
				}
				if account != domain.FeeAccount {
					market, _ := s.repoManager.MarketRepository().GetMarketByName(
						context.Background(), account,
					)
					if market != nil {
						balance, _ := s.wallet.Account().GetBalance(
							context.Background(), account,
						)
						baseThreshold := market.FixedFee.BaseAsset
						quoteThreshold := market.FixedFee.QuoteAsset
						isLowBalance := len(balance) <= 0 ||
							balance[market.BaseAsset] == nil ||
							balance[market.QuoteAsset] == nil ||
							balance[market.BaseAsset].GetTotalBalance() <= baseThreshold ||
							balance[market.QuoteAsset].GetTotalBalance() <= quoteThreshold
						if isLowBalance {
							publishTopic(market.Name, balance, &marketInfo{*market, nil})
						}
					}
				}

				bal, _ := s.GetFeeBalance(context.Background())
				if bal == nil || bal.GetTotalBalance() < s.feeAccountBalanceThreshold {
					balance := map[string]ports.Balance{
						s.wallet.NativeAsset(): bal,
					}
					publishTopic(domain.FeeAccount, balance, nil)
				}
			}
		}
		return false
	}
}

func (s *service) classifyAndStoreTx() func(ports.WalletTxNotification) bool {
	return func(notification ports.WalletTxNotification) bool {
		tx, _ := transaction.NewTxFromHex(notification.GetTxHex())
		txid := tx.TxHash().String()
		ctx := context.Background()
		eventType := notification.GetEventType()

		// If the tx is unconfirmed, let's check if a deposit or withdrawal is
		// already stored in db, otherwise add it by reconstructing related info.
		if eventType.IsBroadcasted() || eventType.IsUnconfirmed() {
			deposits := make([]domain.Deposit, 0)
			withdrawals := make([]domain.Withdrawal, 0)
			walletInfo, _ := s.wallet.Wallet().Info(context.Background())
			accountsInfo := walletInfo.GetAccounts()

			for _, accountNamespace := range notification.GetAccountNames() {
				masterBlindingKey := ""
				account := s.accounts.getLabel(accountNamespace)
				for _, info := range accountsInfo {
					if info.GetLabel() == account {
						masterBlindingKey = info.GetMasterBlindingKey()
						break
					}
				}
				txInfo := s.getTxInfo(tx, account, masterBlindingKey)
				if txInfo.isDeposit() {
					deposits = append(deposits, domain.Deposit{
						AccountName:       account,
						TxID:              txid,
						Timestamp:         time.Now().Unix(),
						TotAmountPerAsset: txInfo.depositAmountPerAsset(),
					})
				}
				if txInfo.isWithdrawal() {
					totAmountPerAsset := txInfo.withdrawalAmountPerAsset()
					if account == domain.FeeAccount {
						totAmountPerAsset[s.wallet.NativeAsset()] -= txInfo.fee
					}
					withdrawals = append(withdrawals, domain.Withdrawal{
						AccountName:       account,
						TxID:              txid,
						Timestamp:         time.Now().Unix(),
						TotAmountPerAsset: totAmountPerAsset,
					})
				}
			}

			if len(deposits) > 0 {
				count, err := s.repoManager.DepositRepository().AddDeposits(ctx, deposits)
				if err != nil {
					log.WithError(err).Warn("failed to add deposit txs")
				} else {
					if count > 0 {
						log.Debugf("added %d new deposit(s) for account %s", count, deposits[0].AccountName)

						// Spawn go routine to publish event for account's deposit.
						go func() {
							defer recoverHandler("publishing a deposit")
							for _, deposit := range deposits {
								accountName := deposit.AccountName
								// Skip fragmenters account, we just don't want to publish
								// events for these auxiliary accounts.
								if accountName == domain.FeeFragmenterAccount ||
									accountName == domain.MarketFragmenterAccount {
									continue
								}

								balance, _ := s.wallet.Account().GetBalance(ctx, deposit.AccountName)
								var market ports.Market = nil
								if accountName != domain.FeeAccount {
									// The wallet is shared: an account that is
									// not one of this daemon's markets (a maker's
									// own account, or any account once the store
									// is closed) has no market to publish with.
									mkt, err := s.repoManager.MarketRepository().GetMarketByName(
										ctx, accountName,
									)
									if err != nil || mkt == nil {
										continue
									}
									market = marketInfo{*mkt, balance}
								}
								if err := s.pubsub.PublisAccountDepositEvent(
									deposit.AccountName, balance, deposit, market,
								); err != nil {
									log.WithError(err).Warn("failed to publish account deposit event")
								}
							}
						}()
					}
				}
			}

			if len(withdrawals) > 0 {
				count, err := s.repoManager.WithdrawalRepository().AddWithdrawals(ctx, withdrawals)
				if err != nil {
					log.WithError(err).Warn("failed to add withdrawal txs")
				} else {
					if count > 0 {
						log.Debugf("added %d new withdrawal(s) for account %s", count, withdrawals[0].AccountName)

						// Spawn go routine to publish event for account's withdrawal.
						go func() {
							defer recoverHandler("publishing a withdrawal")
							for _, withdrawal := range withdrawals {
								accountName := withdrawal.AccountName
								// Skip fragmenters account, we just don't want to publish
								// events for these auxiliary accounts.
								if accountName == domain.FeeFragmenterAccount ||
									accountName == domain.MarketFragmenterAccount {
									continue
								}

								balance, _ := s.wallet.Account().GetBalance(ctx, withdrawal.AccountName)
								var market ports.Market = nil
								if accountName != domain.FeeAccount {
									// The wallet is shared: an account that is
									// not one of this daemon's markets (a maker's
									// own account, or any account once the store
									// is closed) has no market to publish with.
									mkt, err := s.repoManager.MarketRepository().GetMarketByName(
										ctx, accountName,
									)
									if err != nil || mkt == nil {
										continue
									}
									market = marketInfo{*mkt, balance}
								}
								if err := s.pubsub.PublisAccountWithdrawEvent(
									withdrawal.AccountName, balance, withdrawal, market,
								); err != nil {
									log.WithError(err).Warn("failed to publish account withdraw event")
								}
							}
						}()
					}
				}
			}
		}

		return false
	}
}

func (s *service) getTxInfo(
	tx *transaction.Transaction, account, masterBlindingKey string,
) txInfo {
	ctx := context.Background()
	ownedInputs := make([]txOutputInfo, 0)
	notOwnedInputs := make([]txOutputInfo, 0)
	ownedOutputs := make([]txOutputInfo, 0)
	notOwnedOutputs := make([]txOutputInfo, 0)
	addresses, _ := s.wallet.Account().ListAddresses(ctx, account)
	buf, _ := hex.DecodeString(masterBlindingKey)
	masterKey, _ := slip77.FromMasterKey(buf)

	findOwnedUtxo := func(out *transaction.TxOutput) *txOutputInfo {
		for _, addr := range addresses {
			script, _ := seqnet.ToOutputScript(addr, &s.network)
			if bytes.Equal(script, out.Script) {
				// A transparent output needs no key. A confidential one
				// needs the account's blinding key, which is missing while
				// the wallet is locked or unreachable: then it cannot be
				// read, and it is not counted rather than crashing.
				var blindKey []byte
				if out.IsConfidential() {
					if masterKey == nil {
						return nil
					}
					key, _, err := masterKey.DeriveKey(out.Script)
					if err != nil || key == nil {
						return nil
					}
					blindKey = key.Serialize()
				}
				unblinded, _ := confidential.UnblindOutputWithKey(out, blindKey)
				if unblinded != nil {
					return &txOutputInfo{
						hex.EncodeToString(elementsutil.ReverseBytes(unblinded.Asset)),
						unblinded.Value,
					}
				}
			}
		}
		return nil
	}

	for _, in := range tx.Inputs {
		txid := elementsutil.TxIDFromBytes(in.Hash)
		txHex, _ := s.wallet.Transaction().GetTransaction(ctx, txid)
		if txHex == "" {
			continue
		}
		prevoutTx, err := transaction.NewTxFromHex(txHex)
		if err != nil || int(in.Index) >= len(prevoutTx.Outputs) {
			notOwnedInputs = append(notOwnedInputs, txOutputInfo{})
			continue
		}
		prevout := prevoutTx.Outputs[in.Index]
		if ownedInput := findOwnedUtxo(prevout); ownedInput != nil {
			ownedInputs = append(ownedInputs, *ownedInput)
		} else {
			notOwnedInputs = append(notOwnedInputs, txOutputInfo{})
		}
	}

	var fee uint64
	for _, out := range tx.Outputs {
		if len(out.Script) <= 0 {
			fee, _ = elementsutil.ValueFromBytes(out.Value)
			continue
		}
		if ownedOutput := findOwnedUtxo(out); ownedOutput != nil {
			ownedOutputs = append(ownedOutputs, *ownedOutput)
		} else {
			notOwnedOutputs = append(notOwnedOutputs, txOutputInfo{})
		}
	}

	return txInfo{
		account, *tx,
		ownedInputs, notOwnedInputs, ownedOutputs, notOwnedOutputs, fee,
	}
}
