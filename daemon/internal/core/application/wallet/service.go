package wallet

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"sync"

	"github.com/aejkcs50/seqdex/daemon/internal/core/domain"
	"github.com/aejkcs50/seqdex/daemon/internal/core/ports"
	"github.com/aejkcs50/seqdex/daemon/pkg/seqnet"
	"github.com/vulpemventures/go-elements/elementsutil"
	"github.com/vulpemventures/go-elements/network"
	"github.com/vulpemventures/go-elements/psetv2"
	"github.com/vulpemventures/go-elements/slip77"
)

type Service struct {
	wallet     ports.WalletService
	staticInfo ports.WalletInfo

	// rates resolves open-fee-market exchange rates so same-chain swaps can pay
	// the network fee in the transacted asset. Nil when no node RPC is
	// configured, in which case swaps fall back to the native fee asset.
	rates feeRateProvider

	txNotificationHandlers   txNotificationQueue
	utxoNotificationHandlers utxoNotificationQueue
}

// NewService builds the wallet service. nodeRPC is an optional Sequentia node
// JSON-RPC url (http://user:pass@host:port) used to read the open fee-market
// exchange rates; when empty, the open fee market is disabled and swaps pay the
// network fee in the native asset (legacy behavior).
func NewService(wallet ports.WalletService, nodeRPC string) (*Service, error) {
	if wallet == nil {
		return nil, fmt.Errorf("missing ocean wallet service")
	}

	info, err := wallet.Wallet().Info(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to connect to ocean wallet: %s", err)
	}

	var rates feeRateProvider
	if nodeRPC != "" {
		nr, err := newNodeFeeRates(nodeRPC)
		if err != nil {
			return nil, fmt.Errorf("failed to init fee-rate provider: %s", err)
		}
		if nr != nil {
			rates = nr
		}
	}

	txHandlers := txNotificationQueue{
		&sync.Mutex{}, make([]func(ports.WalletTxNotification) bool, 0),
	}
	utxoHandlers := utxoNotificationQueue{
		&sync.Mutex{}, make([]func(ports.WalletUtxoNotification) bool, 0),
	}

	svc := &Service{wallet, info, rates, txHandlers, utxoHandlers}
	go svc.listenToTxNotifications()
	go svc.listenToUtxoNotifications()

	return svc, nil
}

func (s *Service) Wallet() ports.Wallet {
	return s.wallet.Wallet()
}

func (s *Service) Account() ports.Account {
	return s.wallet.Account()
}

func (s *Service) Transaction() ports.Transaction {
	return s.wallet.Transaction()
}

func (s *Service) Notification() ports.Notification {
	return s.wallet.Notification()
}

// Network returns the Sequentia network parameters for the network the ocean
// wallet reports. The ocean-wallet adapter stringifies the wallet's network
// enum to "mainnet"/"testnet"/"regtest"; seqnet.ByName also accepts the
// Sequentia names and "liquid", so the mapping is robust regardless of the
// exact spelling. The native (policy) asset is genesis-derived and supplied at
// runtime from the wallet's GetInfo.nativeAsset (read from the node's
// getsidechaininfo.pegged_asset). Falls back to Sequentia mainnet if the signal
// is unrecognised.
func (s *Service) Network() network.Network {
	net, ok := seqnet.ByName(s.staticInfo.GetNetwork())
	if !ok {
		net = seqnet.SequentiaMainnet
	}
	net.AssetID = s.staticInfo.GetNativeAsset()
	return net
}

func (s *Service) NativeAsset() string {
	return s.staticInfo.GetNativeAsset()
}

func (s *Service) SendToMany(
	account string, outs []ports.TxOutput, msatsPerByte uint64,
) (string, error) {
	ctx := context.Background()
	net := s.Network()
	txManager := s.wallet.Transaction()
	accountManager := s.wallet.Account()
	changeAmountPerAsset := make(map[string]uint64)
	inputs := make([]ports.TxInput, 0)
	outputs := append([]ports.TxOutput{}, outs...)

	for asset, amount := range totOutputAmountPerAsset(outs) {
		utxos, change, _, err := txManager.SelectUtxos(ctx, account, asset, amount)
		if err != nil {
			return "", err
		}
		if change > 0 {
			changeAmountPerAsset[asset] = change
		}
		for _, u := range utxos {
			txid, _ := elementsutil.TxIDToBytes(u.GetTxid())
			var scriptSigSize, witnessSize int
			if len(u.GetRedeemScript()) > 0 {
				scriptSigSize = 35
				witnessSize = 223
			}
			inputs = append(inputs, input{
				txid, u.GetIndex(), u.GetScript(), scriptSigSize, witnessSize,
			})
		}
	}

	if numOfAddress := len(changeAmountPerAsset); numOfAddress > 0 {
		addresses, err := accountManager.DeriveChangeAddresses(
			ctx, account, numOfAddress,
		)
		if err != nil {
			return "", err
		}

		i := 0
		for asset, amount := range changeAmountPerAsset {
			info, err := seqnet.FromAny(addresses[i], &net)
			if err != nil {
				return "", fmt.Errorf("change address %q: %w", addresses[i], err)
			}
			outputs = append(outputs, output{
				asset, amount, hex.EncodeToString(info.Script),
				hex.EncodeToString(info.BlindingKey),
			})
			i++
		}
	}

	dummyFeeAmount, err := txManager.EstimateFees(
		ctx, inputs, outputs, msatsPerByte,
	)
	if err != nil {
		return "", err
	}
	// 150 is an over estimation of an extra confidential output (change).
	dummyFeeAmount += 150
	lbtc := s.staticInfo.GetNativeAsset()
	feeUtxos, change, _, err := txManager.SelectUtxos(
		ctx, domain.FeeAccount, lbtc, dummyFeeAmount,
	)
	if err != nil {
		return "", err
	}

	for _, u := range feeUtxos {
		txid, _ := elementsutil.TxIDToBytes(u.GetTxid())
		inputs = append(inputs, input{txid, u.GetIndex(), u.GetScript(), 0, 0})
	}
	feeAmount := dummyFeeAmount
	if change > 0 {
		addresses, err := accountManager.DeriveAddresses(ctx, domain.FeeAccount, 1)
		if err != nil {
			return "", err
		}
		info, err := seqnet.FromAny(addresses[0], &net)
		if err != nil {
			return "", fmt.Errorf("fee change address %q: %w", addresses[0], err)
		}
		outputs = append(outputs, output{
			lbtc, change, hex.EncodeToString(info.Script),
			hex.EncodeToString(info.BlindingKey),
		})
		feeAmount, err = txManager.EstimateFees(
			ctx, inputs, outputs, msatsPerByte,
		)
		if err != nil {
			return "", err
		}

		changeOut := outputs[len(outputs)-1]
		changeOutScript := changeOut.(output).script
		changeOutBlindKey := changeOut.(output).blindKey
		diff := dummyFeeAmount - feeAmount
		amount := changeOut.GetAmount() + diff
		outputs[len(outputs)-1] = output{
			changeOut.GetAsset(), amount, changeOutScript, changeOutBlindKey,
		}
	}

	outputs = append(outputs, output{lbtc, feeAmount, "", ""})

	pset, err := txManager.CreatePset(ctx, inputs, outputs)
	if err != nil {
		return "", err
	}

	blindedPset, err := txManager.BlindPset(ctx, pset, nil)
	if err != nil {
		return "", err
	}
	txhex, err := txManager.SignPset(ctx, blindedPset, true)
	if err != nil {
		return "", err
	}
	txid, err := txManager.BroadcastTransaction(ctx, txhex)
	if err != nil {
		return "", err
	}
	return txid, nil
}

func (s *Service) CompleteSwap(
	account string, swapRequest ports.SwapRequest, msatsPerByte uint64,
	feesToAdd bool, blind bool,
) (string, []ports.Utxo, int64, error) {
	ctx := context.Background()
	// Confidential is opt-in in Sequentia: blind=false finalizes an EXPLICIT
	// swap (the maker's own outputs carry no blinding key and BlindPset is
	// skipped). blind=true reproduces today's fully-confidential behavior
	// byte-for-byte; the Elements network fee output is always explicit.
	// The maker's posture follows its coins, not only the flag: a confidential
	// input carries blinding factors that only a blinded output can absorb, so
	// once a selected coin is confidential the maker's own outputs must be
	// blinded or the tx can never balance (the node rejects it value in != value
	// out). An account that derives blinded addresses has the key; one that does
	// not cannot hold such a coin. makerOutKeys remembers each maker output's
	// available key so a late flip (a confidential FEE coin) can still apply it.
	outBlindKey := func(bk []byte) string {
		if !blind {
			return ""
		}
		return hex.EncodeToString(bk)
	}
	var makerOutKeys []string
	spendsConfidential := func(us []ports.Utxo) bool {
		for _, u := range us {
			if isConfidentialBlinder(u.GetAssetBlinder()) || isConfidentialBlinder(u.GetValueBlinder()) {
				return true
			}
		}
		return false
	}
	net := s.Network()
	txManager := s.wallet.Transaction()
	accountManager := s.wallet.Account()
	inputs := make([]ports.TxInput, 0)
	existingInputs := make([]ports.TxInput, 0)
	existingOutputs := make([]ports.TxOutput, 0)

	ptx, err := psetv2.NewPsetFromBase64(swapRequest.GetTransaction())
	if err != nil {
		return "", nil, 0, fmt.Errorf("parse swap request PSET: %w", err)
	}
	if ptx == nil {
		return "", nil, 0, fmt.Errorf("swap request carried no transaction")
	}
	for _, in := range ptx.Inputs {
		var scriptSigSize, witnessSize int
		if len(in.RedeemScript) > 0 {
			// values for 2of2 native bare multisig inputs
			scriptSigSize = 223
		}
		if len(in.WitnessScript) > 0 {
			// values for 2of2 native or wrapped segwit multisig inputs
			if scriptSigSize > 0 {
				scriptSigSize = 35
			}
			witnessSize = 223
		}
		existingInputs = append(existingInputs, input{
			in.PreviousTxid, in.PreviousTxIndex, hex.EncodeToString(in.GetUtxo().Script),
			scriptSigSize, witnessSize,
		})
	}
	for _, out := range ptx.Outputs {
		existingOutputs = append(existingOutputs, output{
			hex.EncodeToString(elementsutil.ReverseBytes(out.Asset)),
			out.Value, hex.EncodeToString(out.Script), hex.EncodeToString(out.BlindingPubkey),
		})
	}

	// Open fee market (taker-funded network fee): detect an explicit, empty-script
	// Elements fee output the taker added to the swap PSET, in any asset. When
	// present the maker validates it below and adds NO fee output of its own.
	// Exactly one is allowed (more would under-state the fee); a fee in the
	// received asset (assetR) is rejected — it would inflate the assetR sum the
	// price/amount checks compare to amount_r and let the taker skim the maker.
	takerFeeAsset := ""
	var takerFeeValue uint64
	emptyScriptOuts := 0
	for _, out := range ptx.Outputs {
		if len(out.Script) == 0 {
			emptyScriptOuts++
			takerFeeAsset = hex.EncodeToString(elementsutil.ReverseBytes(out.Asset))
			takerFeeValue = out.Value
		}
	}
	if emptyScriptOuts > 1 {
		return "", nil, -1, fmt.Errorf(
			"swap request carries %d fee outputs; at most one is allowed",
			emptyScriptOuts,
		)
	}
	takerFundsFee := emptyScriptOuts == 1
	if takerFundsFee && takerFeeAsset == swapRequest.GetAssetR() {
		return "", nil, -1, fmt.Errorf(
			"swap fee output cannot be denominated in the received asset",
		)
	}

	amountR := swapRequest.GetAmountR()
	if swapRequest.GetFeeAsset() == swapRequest.GetAssetR() && !feesToAdd {
		amountR -= swapRequest.GetFeeAmount()
	}

	utxos, change, unlockTime, err := txManager.SelectUtxos(
		ctx, account, swapRequest.GetAssetR(), amountR,
	)
	if err != nil {
		return "", nil, -1, err
	}
	giveChange := change
	if !blind && spendsConfidential(utxos) {
		blind = true
	}

	for _, u := range utxos {
		txid, _ := elementsutil.TxIDToBytes(u.GetTxid())
		var scriptSigSize, witnessSize int
		if len(u.GetRedeemScript()) > 0 {
			scriptSigSize = 35
			witnessSize = 223
		}
		inputs = append(inputs, input{
			txid, u.GetIndex(), u.GetScript(), scriptSigSize, witnessSize,
		})
	}

	addresses, err := accountManager.DeriveAddresses(ctx, account, 1)
	if err != nil {
		return "", nil, -1, err
	}
	// The account derives TRANSPARENT addresses unless blinded ones were asked
	// for; decoding them as confidential returned nil and the swap panicked on
	// info.Script — every same-chain lift against a transparent maker died here.
	info, err := seqnet.FromAny(addresses[0], &net)
	if err != nil {
		return "", nil, -1, fmt.Errorf("receive address %q: %w", addresses[0], err)
	}
	amountP := swapRequest.GetAmountP()
	if swapRequest.GetFeeAsset() == swapRequest.GetAssetP() && feesToAdd {
		amountP += swapRequest.GetFeeAmount()
	}
	recvKey := s.ownBlindingKey(ctx, account, info.Script, info.BlindingKey)
	if blind && len(recvKey) == 0 {
		return "", nil, -1, fmt.Errorf("account %s spends a confidential coin but derives no blinding key for %s", account, addresses[0])
	}
	outputs := []ports.TxOutput{
		output{
			swapRequest.GetAssetP(), amountP,
			hex.EncodeToString(info.Script), outBlindKey(recvKey),
		},
	}
	makerOutKeys = append(makerOutKeys, hex.EncodeToString(recvKey))
	if change > 0 {
		addresses, err := accountManager.DeriveChangeAddresses(ctx, account, 1)
		if err != nil {
			return "", nil, -1, err
		}
		info, err := seqnet.FromAny(addresses[0], &net)
		if err != nil {
			return "", nil, -1, fmt.Errorf("change address %q: %w", addresses[0], err)
		}
		changeKey := s.ownBlindingKey(ctx, account, info.Script, info.BlindingKey)
		outputs = append(outputs, output{
			swapRequest.GetAssetR(), change, hex.EncodeToString(info.Script),
			outBlindKey(changeKey),
		})
		makerOutKeys = append(makerOutKeys, hex.EncodeToString(changeKey))
	}

	allInputs := append(existingInputs, inputs...)
	allOutputs := append(existingOutputs, outputs...)

	// feeUtxos are the maker's fee-funding UTXOs in the maker-funded path; they
	// stay nil in the taker-funded path (the taker supplied the fee input +
	// output itself) so the append at the end of CompleteSwap is a no-op.
	var feeUtxos []ports.Utxo

	if takerFundsFee {
		// Taker-funded network fee: the explicit fee output is already in the swap
		// PSET (it's in existingOutputs/allOutputs). Validate the fee asset is
		// fee-eligible and its node-floor native-equivalent covers the size-based
		// fee for the FINAL transaction, then add no maker fee input/output.
		rate, eligible, rateErr := s.feeExchangeRate(takerFeeAsset)
		if rateErr != nil {
			return "", nil, -1, rateErr
		}
		if !eligible {
			return "", nil, -1, fmt.Errorf(
				"taker network-fee asset %s is not fee-eligible", takerFeeAsset,
			)
		}
		// Every taker input must be revealed: BlindPset is the last blinder and
		// cannot balance a confidential input whose secrets it does not have.
		revealed := make(map[uint32]bool)
		for _, ui := range swapRequest.GetUnblindedInputs() {
			revealed[ui.GetIndex()] = true
		}
		for i := range existingInputs {
			if !revealed[uint32(i)] {
				return "", nil, -1, fmt.Errorf(
					"taker input %d missing from unblinded_inputs", i,
				)
			}
		}
		requiredFee, err := txManager.EstimateFees(
			ctx, allInputs, allOutputs, msatsPerByte,
		)
		if err != nil {
			return "", nil, -1, err
		}
		// Node-floor valuation, matching the network's
		// native_equivalent = value * rate / exchange_rate_scale (integer floor):
		// require takerFeeValue*rate >= requiredFee*exchangeRateScale. big.Int
		// keeps the multiplication overflow-safe.
		haveFee := new(big.Int).Mul(
			new(big.Int).SetUint64(takerFeeValue), new(big.Int).SetUint64(rate),
		)
		needFee := new(big.Int).Mul(
			new(big.Int).SetUint64(requiredFee), big.NewInt(exchangeRateScale),
		)
		if haveFee.Cmp(needFee) < 0 {
			return "", nil, -1, fmt.Errorf(
				"taker network fee too low: %d atoms of %s do not cover the %d-atom native fee",
				takerFeeValue, takerFeeAsset, requiredFee,
			)
		}
	} else {
		dummyFeeAmount, err := txManager.EstimateFees(
			ctx, allInputs, allOutputs, msatsPerByte,
		)
		if err != nil {
			return "", nil, -1, err
		}

		// 150 is an over estimation of an extra confidential output (change).
		dummyFeeAmount += 150

		// Open fee market: pay the network fee in the asset already being
		// transacted (assetR), valued native-equivalent, rather than always
		// subsidising it in the native asset from the fee account. No asset is
		// privileged. If assetR isn't fee-eligible (not on the node's whitelist, or
		// no node RPC configured), fall back to the native asset + fee account so
		// swaps never break.
		nativeAsset := s.staticInfo.GetNativeAsset()
		feeAssetNet := swapRequest.GetAssetR()
		feeRate, eligible, rateErr := s.feeExchangeRate(feeAssetNet)
		if rateErr != nil {
			// The node could not be asked. Falling through to "not eligible" would
			// re-route the fee through the native asset at par on every transient
			// failure; refuse and let the taker retry instead.
			return "", nil, -1, rateErr
		}
		feeFundAccount := account
		if !eligible {
			// assetR isn't fee-eligible, so `account` (which holds assetR) can't
			// fund a native fee. Fall back to the dedicated fee account in native.
			feeAssetNet = nativeAsset
			feeRate = exchangeRateScale
			feeFundAccount = domain.FeeAccount
		}
		// NOTE: when feeAssetNet == nativeAsset here, assetR IS the native asset, so
		// the market `account` already holds native UTXOs (it funds the give leg
		// from them just above) and MUST fund the native fee from the same account.
		// Routing the native fee through domain.FeeAccount in this case was a
		// vestige of native-asset privilege (tdex assumed a market account never
		// holds the native asset); it violated Sequentia's no-privileged-coin fee
		// market AND broke every native-give swap whenever fee_account's UTXO was
		// stale/spent (-> bad-txns-inputs-missingorspent). We therefore keep
		// feeFundAccount = account for the native-give case, exactly mirroring the
		// working non-native path.

		// Convert the required native-equivalent fee into feeAssetNet (round UP).
		dummyFeeInAsset := feeInAsset(dummyFeeAmount, feeRate)
		var change uint64
		// When the fee is paid in the asset the give leg already spends, from the same
		// account, and that leg's change covers it with room for a non-dust remainder,
		// the fee comes OUT OF THAT CHANGE: no second coin selection. The give leg's
		// coins are locked by the selection above, and Ocean's spendable query excludes
		// locked coins, so an account holding its asset in a single UTXO failed every
		// lift with "insufficient funds" until the coin had been split, and every lift
		// pinned two coins for the lock window.
		const changeDustFloor = 1000
		feeFromGiveChange := feeAssetNet == swapRequest.GetAssetR() && feeFundAccount == account &&
			giveChange > dummyFeeInAsset+changeDustFloor
		if feeFromGiveChange {
			last := outputs[len(outputs)-1].(output)
			change = giveChange - dummyFeeInAsset
			outputs[len(outputs)-1] = output{last.asset, change, last.script, last.blindKey}
		} else {
			feeUtxos, change, _, err = txManager.SelectUtxos(
				ctx, feeFundAccount, feeAssetNet, dummyFeeInAsset,
			)
			if err != nil {
				return "", nil, -1, err
			}
		}

		for _, u := range feeUtxos {
			txid, _ := elementsutil.TxIDToBytes(u.GetTxid())
			inputs = append(inputs, input{txid, u.GetIndex(), u.GetScript(), 0, 0})
		}
		// A confidential fee coin flips the maker to blinded after its outputs
		// were built: re-key them from the keys remembered at derivation.
		if !blind && spendsConfidential(feeUtxos) {
			blind = true
			for i := range outputs {
				if i >= len(makerOutKeys) {
					break
				}
				if makerOutKeys[i] == "" {
					return "", nil, -1, fmt.Errorf("account %s spends a confidential fee coin but derives no blinding key", feeFundAccount)
				}
				o := outputs[i].(output)
				outputs[i] = output{o.asset, o.amount, o.script, makerOutKeys[i]}
			}
		}
		feeAmount := dummyFeeAmount     // native-equivalent fee (atoms)
		feeNetAmount := dummyFeeInAsset // fee vout value, denominated in feeAssetNet
		if change > 0 {
			if !feeFromGiveChange {
				addresses, err := accountManager.DeriveChangeAddresses(
					ctx, feeFundAccount, 1,
				)
				if err != nil {
					return "", nil, -1, err
				}
				info, err := seqnet.FromAny(addresses[0], &net)
				if err != nil {
					return "", nil, -1, fmt.Errorf("fee change address %q: %w", addresses[0], err)
				}
				outputs = append(outputs, output{
					feeAssetNet, change, hex.EncodeToString(info.Script),
					outBlindKey(s.ownBlindingKey(ctx, feeFundAccount, info.Script, info.BlindingKey)),
				})
			}

			allInputs := append(existingInputs, inputs...)
			allOutputs := append(existingOutputs, outputs...)
			feeAmount, err = txManager.EstimateFees(
				ctx, allInputs, allOutputs, msatsPerByte,
			)
			if err != nil {
				return "", nil, -1, err
			}
			feeNetAmount = feeInAsset(feeAmount, feeRate)

			changeOut := outputs[len(outputs)-1]
			changeOutScript := changeOut.(output).script
			changeOutBlindKey := changeOut.(output).blindKey
			// Refund the over-estimation (in feeAssetNet) back to the change output.
			diff := dummyFeeInAsset - feeNetAmount
			amount := changeOut.GetAmount() + diff
			outputs[len(outputs)-1] = output{
				changeOut.GetAsset(), amount, changeOutScript, changeOutBlindKey,
			}
		}

		// Exactly one explicit/unblinded Elements fee output, in feeAssetNet.
		outputs = append(outputs, output{feeAssetNet, feeNetAmount, "", ""})
	}

	pset, err := txManager.UpdatePset(
		ctx, swapRequest.GetTransaction(), inputs, outputs,
	)
	if err != nil {
		return "", nil, -1, err
	}

	// Skip blinding entirely for an explicit swap; otherwise blind exactly as
	// before, using the proposer's revealed unblinded inputs.
	blindedPset := pset
	if blind {
		blindedPset, err = txManager.BlindPset(
			ctx, pset, swapRequest.GetUnblindedInputs(),
		)
		if err != nil {
			return "", nil, -1, err
		}
	}

	signedPset, err := txManager.SignPset(ctx, blindedPset, false)
	if err != nil {
		return "", nil, -1, err
	}

	utxos = append(utxos, feeUtxos...)

	return signedPset, utxos, unlockTime, nil
}

// isConfidentialBlinder reports whether a utxo blinder (hex) is a real blinding
// factor. Ocean reports an explicit coin's blinders empty or all zero.
// ownBlindingKey is the blinding pubkey for one of the account's own outputs: the address's
// own key when it is confidential, else the key the wallet derives for that script from the
// account's master blinding key (SLIP77) -- the key it reads that script's blinded outputs
// with. An account that hands out transparent addresses still holds the blinded coins it
// received before, and spending one needs blinded outputs to absorb its blinding factors.
// Without a key here a maker refused every swap such a coin was selected for ("spends a
// confidential coin but derives no blinding key"), and re-posted the offer to fail again.
// Nil when the account's master blinding key is not known.
func (s *Service) ownBlindingKey(
	ctx context.Context, account string, script, given []byte,
) []byte {
	if len(given) > 0 {
		return given
	}
	info, err := s.wallet.Wallet().Info(ctx)
	if err != nil || info == nil {
		return nil
	}
	for _, a := range info.GetAccounts() {
		if a.GetLabel() == account || a.GetNamespace() == account {
			return blindingKeyFor(a.GetMasterBlindingKey(), script)
		}
	}
	return nil
}

// blindingKeyFor is the SLIP77 blinding pubkey (compressed) of script under the master
// blinding key in hex, or nil when that key is missing or malformed.
func blindingKeyFor(masterBlindingKeyHex string, script []byte) []byte {
	buf, err := hex.DecodeString(masterBlindingKeyHex)
	if err != nil || len(buf) == 0 {
		return nil
	}
	master, err := slip77.FromMasterKey(buf)
	if err != nil {
		return nil
	}
	_, pub, err := master.DeriveKey(script)
	if err != nil || pub == nil {
		return nil
	}
	return pub.SerializeCompressed()
}

func isConfidentialBlinder(hexBlinder string) bool {
	if hexBlinder == "" {
		return false
	}
	for _, c := range hexBlinder {
		if c != '0' {
			return true
		}
	}
	return false
}

func (s *Service) RegisterHandlerForTxEvent(
	handler func(ports.WalletTxNotification) bool,
) {
	s.txNotificationHandlers.pushBack(handler)
}

func (s *Service) RegisterHandlerForUtxoEvent(
	handler func(ports.WalletUtxoNotification) bool,
) {
	s.utxoNotificationHandlers.pushBack(handler)
}

func (s *Service) Close() {
	s.wallet.Close()
}

func (s *Service) listenToTxNotifications() {
	for notification := range s.wallet.Notification().GetTxNotifications() {
		toRepeat := make([]func(ports.WalletTxNotification) bool, 0)
		numHandlers := s.txNotificationHandlers.len()
		for i := 0; i < numHandlers; i++ {
			handler := s.txNotificationHandlers.pop()
			done := handler(notification)
			if !done {
				toRepeat = append(toRepeat, handler)
			}
		}
		for _, handler := range toRepeat {
			s.txNotificationHandlers.pushBack(handler)
		}
	}
}

func (s *Service) listenToUtxoNotifications() {
	for notification := range s.wallet.Notification().GetUtxoNotifications() {
		toRepeat := make([]func(ports.WalletUtxoNotification) bool, 0)
		numHandlers := s.utxoNotificationHandlers.len()
		for i := 0; i < numHandlers; i++ {
			handler := s.utxoNotificationHandlers.pop()
			done := handler(notification)
			if !done {
				toRepeat = append(toRepeat, handler)
			}
		}
		for _, handler := range toRepeat {
			s.utxoNotificationHandlers.pushBack(handler)
		}
	}
}
