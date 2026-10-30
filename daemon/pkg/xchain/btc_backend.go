package xchain

import (
	"sync"

	"fmt"

	"github.com/btcsuite/btcd/chaincfg"
)

// btcBackend abstracts the maker's BTC-leg (parent / anchor-source) operations
// so the orchestrator + maker can run the BTC leg in EITHER transaction format:
//
//   - Elements format (elementsBTCBackend): the parent is an Elements-mode node.
//     This is the original behaviour, used to verify the mechanism against an
//     Elements parent (asset commitments, Elements serialization, decodescript).
//
//   - Bitcoin format (bitcoinBTCBackend): the parent is a REAL bitcoind (regtest
//     or testnet4). Values are 8-byte sats, no asset commitments, standard
//     legacy-P2SH spends, txs parsed/built with btcd's wire/txscript. This is
//     the "real-bitcoind leg" the maker needs to verify + claim a genuine
//     Bitcoin HTLC.
//
// The SEQ leg is ALWAYS Elements-format (the anchored Sequentia node), so only
// the BTC side is pluggable. The HTLC redeemScript is generic Bitcoin Script and
// byte-identical across both backends — only the tx envelope/sighash differ.
type btcBackend interface {
	// HTLCScript renders the leg-agnostic redeemScript.
	HTLCScript(claimPub, refundPub []byte, locktime uint32) ([]byte, error)

	// LockBTCLeg funds the HTLC P2SH with amountCoins of the BTC asset and
	// returns the funded leg + the parent height at which it confirmed (Hp). Used
	// by the in-process orchestrator demo where one process plays both roles.
	LockBTCLeg(script []byte, amountCoins string, locktime uint32) (*LegLock, int64, error)

	// VerifyBTCLeg checks the taker's funded BTC leg matches the agreed params
	// (recomputes the script from H/claim/refund/locktime, locates the funded
	// P2SH output, checks value + confirmations) and returns a LegLock the maker
	// can later claim.
	VerifyBTCLeg(
		hashH, makerClaimPub, takerRefundPub, providedScript []byte,
		btcLocktime uint32,
		txid string, vout uint32, amount uint64, assetID string,
		minConf int,
	) (*VerifiedBTCLeg, error)

	// ClaimBTCLeg spends the HTLC via the redeem/IF branch (revealing the
	// preimage) to a fresh maker address, broadcasts it, and returns the txid.
	ClaimBTCLeg(leg *LegLock, claimKey *Key, fee uint64) (string, error)

	// RefundBTCLeg spends the HTLC via the refund/ELSE (CLTV) branch back to a
	// fresh maker address at the given nLockTime, broadcasts it, and returns the
	// txid. Used by the REVERSE (asset->BTC) maker — which funds the BTC leg — to
	// reclaim it after btcLocktime if the taker never funds/claims the SEQ leg.
	RefundBTCLeg(leg *LegLock, refundKey *Key, nLockTime uint32, fee uint64) (string, error)
}

// --- Elements BTC backend (original behaviour) ------------------------------

// elementsBTCBackend runs the BTC leg against an Elements-mode parent, reusing
// the Chain (chain.go) + ElementsLeg (leg_elements.go) that verified the
// mechanism originally.
type elementsBTCBackend struct {
	chain  *Chain
	leg    *ElementsLeg
	spends spendCache
	// lockAsset is the asset LockBTCLeg funds the HTLC with ("" = the pegged
	// bitcoin asset, the original behaviour). Set (via newElementsBTCBackendAsset)
	// when this "BTC-position" leg is actually a Sequentia ISSUED asset — the
	// mixed same-chain shape (rails 7/8), where the quote asset stands in BTC's
	// structural place. Verify/Claim were always asset-aware (the caller passes
	// the expected asset id; the claim spends whatever asset the leg holds).
	lockAsset string
}

func newElementsBTCBackend(chain *Chain, prim LockPrimitive) *elementsBTCBackend {
	return &elementsBTCBackend{chain: chain, leg: NewElementsLeg(LegBTC, prim)}
}

// newElementsBTCBackendAsset is newElementsBTCBackend with the funding asset for
// LockBTCLeg pinned to an issued asset id (the mixed same-chain quote leg).
func newElementsBTCBackendAsset(chain *Chain, prim LockPrimitive, lockAsset string) *elementsBTCBackend {
	return &elementsBTCBackend{chain: chain, leg: NewElementsLeg(LegBTC, prim), lockAsset: lockAsset}
}

func (b *elementsBTCBackend) HTLCScript(claimPub, refundPub []byte, locktime uint32) ([]byte, error) {
	return b.leg.HTLCScript(claimPub, refundPub, locktime)
}

func (b *elementsBTCBackend) LockBTCLeg(script []byte, amountCoins string, locktime uint32) (*LegLock, int64, error) {
	funded, err := b.chain.LockHTLC(script, amountCoins, b.lockAsset) // "" => pegged bitcoin asset
	if err != nil {
		return nil, 0, err
	}
	// Regtest harness convenience: mine the funding in and report its height.
	// On a LIVE network (the PoS testnet/mainnet cannot generatetoaddress) this
	// fails — that is not an error: return height 0 (broadcast-only) and let
	// the caller poll confirmations, exactly the bitcoin backend's live path.
	if err := b.chain.Mine(1); err != nil {
		return &LegLock{Script: script, Funded: funded, Locktime: locktime}, 0, nil
	}
	hp, err := b.chain.BlockCount()
	if err != nil {
		return nil, 0, err
	}
	return &LegLock{Script: script, Funded: funded, Locktime: locktime}, hp, nil
}

func (b *elementsBTCBackend) VerifyBTCLeg(
	hashH, makerClaimPub, takerRefundPub, providedScript []byte,
	btcLocktime uint32,
	txid string, vout uint32, amount uint64, assetID string,
	minConf int,
) (*VerifiedBTCLeg, error) {
	want, err := b.leg.MatchHTLCScript(providedScript, makerClaimPub, takerRefundPub, btcLocktime)
	if err != nil {
		return nil, err
	}
	if want == nil {
		cur, _ := b.leg.HTLCScript(makerClaimPub, takerRefundPub, btcLocktime)
		return nil, fmt.Errorf("%w: redeemScript mismatch (want %x, got %x)", ErrBTCLegInvalid, cur, providedScript)
	}
	wantP2SH, err := b.chain.P2SHAddress(want)
	if err != nil {
		return nil, err
	}
	out, err := b.chain.OutputAt(txid, vout)
	if err != nil {
		// A node that has not SEEN the tx yet (propagation lag on a fresh 0-conf
		// leg) is the retryable not-found class, not an invalid leg.
		if isTxNotFoundRPC(err) {
			return nil, fmt.Errorf("%w: funding tx %s: %v", ErrBTCLegNotSeen, txid, err)
		}
		return nil, fmt.Errorf("%w: %v", ErrBTCLegInvalid, err)
	}
	wantSPK, err := b.chain.AddressScriptPubKey(wantP2SH)
	if err != nil {
		return nil, err
	}
	if out.ScriptPubKeyHex != wantSPK {
		return nil, fmt.Errorf("%w: vout %d does not pay the HTLC P2SH", ErrBTCLegInvalid, vout)
	}
	if out.ValueAtoms != amount {
		return nil, fmt.Errorf("%w: btc-leg value %d != quoted %d", ErrBTCLegInvalid, out.ValueAtoms, amount)
	}
	if assetID != "" && out.AssetID != assetID {
		return nil, fmt.Errorf("%w: btc-leg asset %s != %s", ErrBTCLegInvalid, out.AssetID, assetID)
	}
	confs, err := b.chain.TxConfirmations(txid)
	if err != nil {
		// Same not-found split as OutputAt (the tx can be evicted between the
		// two lookups): not-seen is retryable, everything else is invalid.
		if isTxNotFoundRPC(err) {
			return nil, fmt.Errorf("%w: funding tx %s: %v", ErrBTCLegNotSeen, txid, err)
		}
		return nil, fmt.Errorf("%w: %v", ErrBTCLegInvalid, err)
	}
	if confs < minConf {
		return nil, fmt.Errorf("%w: btc-leg has %d confirmations, need %d", ErrBTCLegUnconfirmed, confs, minConf)
	}
	height, err := b.chain.BlockCount()
	if err != nil {
		return nil, err
	}
	legHeight := height - int64(confs) + 1
	return &VerifiedBTCLeg{
		Leg: &LegLock{
			Script:   want,
			Funded:   &FundedHTLC{TxID: txid, Vout: vout, Amount: out.ValueAtoms, AssetID: out.AssetID},
			Locktime: btcLocktime,
		},
		Height:         legHeight,
		Confirmations:  confs,
		ExpectedScript: want,
	}, nil
}

func (b *elementsBTCBackend) ClaimBTCLeg(leg *LegLock, claimKey *Key, fee uint64) (string, error) {
	dest, err := b.chain.NewDestScript()
	if err != nil {
		return "", err
	}
	txid, err := b.spends.once(b.chain, leg.Funded.TxID, leg.Funded.Vout, "claim", func() (string, error) {
		return b.leg.Redeem(leg.Script, ElementsSpendInput{
			TxID:    leg.Funded.TxID,
			Vout:    leg.Funded.Vout,
			Amount:  leg.Funded.Amount,
			AssetID: leg.Funded.AssetID,
			DestSPK: dest,
			Fee:     fee,
		}, claimKey)
	})
	if err != nil {
		return "", err
	}
	// Regtest harness convenience only: on a live network the mine fails, but
	// the claim is ALREADY broadcast — failing here would misreport a
	// successful claim as retryable. The network mines it.
	_ = b.chain.Mine(1)
	return txid, nil
}

func (b *elementsBTCBackend) RefundBTCLeg(leg *LegLock, refundKey *Key, nLockTime uint32, fee uint64) (string, error) {
	dest, err := b.chain.NewDestScript()
	if err != nil {
		return "", err
	}
	txid, err := b.spends.once(b.chain, leg.Funded.TxID, leg.Funded.Vout, "refund", func() (string, error) {
		return b.leg.Refund(leg.Script, ElementsSpendInput{
			TxID:    leg.Funded.TxID,
			Vout:    leg.Funded.Vout,
			Amount:  leg.Funded.Amount,
			AssetID: leg.Funded.AssetID,
			DestSPK: dest,
			Fee:     fee,
		}, nLockTime, refundKey)
	})
	if err != nil {
		return "", err
	}
	// Same live-network rule as ClaimBTCLeg: the refund is already broadcast.
	_ = b.chain.Mine(1)
	return txid, nil
}

// --- Bitcoin BTC backend (the real-bitcoind leg) ----------------------------

// bitcoinBTCBackend runs the BTC leg against a REAL bitcoind (regtest/testnet4),
// using BitcoinChain (chain_bitcoin.go) + BitcoinLeg (leg_bitcoin.go). This is
// the deferred "real-bitcoind-leg": the maker verifies + claims a genuine
// Bitcoin HTLC in Bitcoin transaction format.
type bitcoinBTCBackend struct {
	chain  *BitcoinChain
	leg    *BitcoinLeg
	spends spendCache
	keep   *btcKeeper
	once   sync.Once
}

func (b *bitcoinBTCBackend) keeper() *btcKeeper {
	b.once.Do(func() { b.keep = newBTCKeeper(b.chain) })
	return b.keep
}

func newBitcoinBTCBackend(chain *BitcoinChain, prim LockPrimitive) *bitcoinBTCBackend {
	return &bitcoinBTCBackend{chain: chain, leg: NewBitcoinLeg(prim, chain.Params())}
}

func (b *bitcoinBTCBackend) HTLCScript(claimPub, refundPub []byte, locktime uint32) ([]byte, error) {
	return b.leg.HTLCScript(claimPub, refundPub, locktime)
}

// LockBTCLeg funds the HTLC via bitcoind sendtoaddress to the locally-derived
// P2SH address and returns the funded leg + the parent height Hp at which it
// confirmed.
//
// On regtest there are no miners, so it mines the funding tx into a block itself
// (the leg confirms immediately and Hp is the resulting height). On a live
// network (testnet4/mainnet) there is no on-demand mining: it only broadcasts,
// the tx sits at 0 confirmations, and Hp is returned as 0 because the funding
// height is not yet known. The maker's watcher polls for the confirmation and
// records the real Hp later (advanceReverse), which gates when the taker may fund
// the anchored SEQ leg.
//
// In the REVERSE (asset->BTC) flow the maker funds the BTC leg via this path; in
// the forward flow the taker funds the BTC leg and the maker only
// VerifyBTCLeg/ClaimBTCLeg.
func (b *bitcoinBTCBackend) LockBTCLeg(script []byte, amountCoins string, locktime uint32) (*LegLock, int64, error) {
	addr, err := b.leg.P2SHAddress(script)
	if err != nil {
		return nil, 0, err
	}
	// The going rate (GoingRate: the live mempool, never under the policy's floor
	// or SetFeeRate), so funding never waits on a stale estimate; the keeper
	// lifts it by CPFP while it waits, keeping its txid, which the other side
	// has been told.
	var txid string
	rate, _ := b.chain.GoingRate()
	named := map[string]interface{}{"address": addr, "amount": amountCoins, "fee_rate": rate}
	if err := b.chain.RPC().CallNamed(&txid, "sendtoaddress", named); err != nil {
		return nil, 0, err
	}
	b.keeper().watchFunding(txid)

	// Only regtest can confirm the funding tx on demand; a real network must wait.
	regtest := b.chain.Params() == &chaincfg.RegressionNetParams
	if regtest {
		var mineAddr string
		if err := b.chain.RPC().Call(&mineAddr, "getnewaddress"); err != nil {
			return nil, 0, err
		}
		var hashes []string
		if err := b.chain.RPC().Call(&hashes, "generatetoaddress", 1, mineAddr); err != nil {
			return nil, 0, err
		}
	}

	// Locate the funded vout by matching the P2SH scriptPubKey in the raw tx.
	// RawTxAndConfirmations resolves the tx at 0 confirmations too (txindex), so
	// this works whether or not we mined above.
	rawHex, _, err := b.chain.RawTxAndConfirmations(txid)
	if err != nil {
		return nil, 0, err
	}
	vout, amount, err := b.locateHTLCOutput(rawHex, script)
	if err != nil {
		return nil, 0, err
	}

	// Hp is the confirmation height: known on regtest (we just mined it), unknown
	// (0) on a live network until the tx confirms and the watcher records it.
	var hp int64
	if regtest {
		if hp, err = b.chain.BlockCount(); err != nil {
			return nil, 0, err
		}
	}
	return &LegLock{
		Script:   script,
		Funded:   &FundedHTLC{TxID: txid, Vout: vout, Amount: amount, AssetID: ""},
		Locktime: locktime,
	}, hp, nil
}

func (b *bitcoinBTCBackend) VerifyBTCLeg(
	hashH, makerClaimPub, takerRefundPub, providedScript []byte,
	btcLocktime uint32,
	txid string, vout uint32, amount uint64, assetID string,
	minConf int,
) (*VerifiedBTCLeg, error) {
	// The script the taker sent must be one of the accepted forms for these
	// parameters (the guarded current form, or the legacy form an older peer built).
	want, err := b.leg.MatchHTLCScript(providedScript, makerClaimPub, takerRefundPub, btcLocktime)
	if err != nil {
		return nil, err
	}
	if want == nil {
		cur, _ := b.leg.HTLCScript(makerClaimPub, takerRefundPub, btcLocktime)
		return nil, fmt.Errorf("%w: redeemScript mismatch (want %x, got %x)", ErrBTCLegInvalid, cur, providedScript)
	}
	// Fetch the raw funding tx (Bitcoin format) + confirmations and verify it.
	rawHex, confs, err := b.chain.RawTxAndConfirmations(txid)
	if err != nil {
		// A node that has not SEEN the tx yet (propagation lag on a fresh 0-conf
		// leg) is the retryable not-found class, not an invalid leg.
		if isTxNotFoundRPC(err) {
			return nil, fmt.Errorf("%w: funding tx %s: %v", ErrBTCLegNotSeen, txid, err)
		}
		return nil, fmt.Errorf("%w: fetch funding tx %s: %v", ErrBTCLegInvalid, txid, err)
	}
	funded, err := b.leg.VerifyFundedHTLCScript(rawHex, want, hashH, amount, confs, minConf)
	if err != nil {
		return nil, err
	}
	// The taker quoted (txid, vout); the verifier located the HTLC output by its
	// scriptPubKey. They must agree (defends against a mis-pointed outpoint).
	if funded.Vout != vout {
		return nil, fmt.Errorf("%w: HTLC output at vout %d, taker claimed vout %d", ErrBTCLegInvalid, funded.Vout, vout)
	}
	height, err := b.chain.BlockCount()
	if err != nil {
		return nil, err
	}
	legHeight := height - int64(confs) + 1
	return &VerifiedBTCLeg{
		Leg: &LegLock{
			Script:   want,
			Funded:   &FundedHTLC{TxID: funded.TxID, Vout: funded.Vout, Amount: funded.Amount, AssetID: ""},
			Locktime: btcLocktime,
		},
		Height:         legHeight,
		Confirmations:  confs,
		ExpectedScript: want,
	}, nil
}

func (b *bitcoinBTCBackend) ClaimBTCLeg(leg *LegLock, claimKey *Key, fee uint64) (string, error) {
	dest, err := b.chain.NewDestScript()
	if err != nil {
		return "", err
	}
	build := func(f uint64) (string, error) {
		return b.leg.BuildClaimTx(leg.Script, BitcoinSpendInput{
			TxID:   leg.Funded.TxID,
			Vout:   leg.Funded.Vout,
			Amount: leg.Funded.Amount,
			DestPK: dest,
			Fee:    f,
		}, claimKey)
	}
	return b.spend(leg, "claim", fee, build)
}

func (b *bitcoinBTCBackend) RefundBTCLeg(leg *LegLock, refundKey *Key, nLockTime uint32, fee uint64) (string, error) {
	dest, err := b.chain.NewDestScript()
	if err != nil {
		return "", err
	}
	build := func(f uint64) (string, error) {
		return b.leg.BuildRefundTx(leg.Script, BitcoinSpendInput{
			TxID:   leg.Funded.TxID,
			Vout:   leg.Funded.Vout,
			Amount: leg.Funded.Amount,
			DestPK: dest,
			Fee:    f,
		}, nLockTime, refundKey)
	}
	return b.spend(leg, "refund", fee, build)
}

// spend builds an HTLC claim or refund at the going rate (the caller's fee
// when that is more; never over half the output), broadcasts it once per
// outpoint (spendCache), and hands it to the keeper, which bumps it (RBF:
// both spends signal it) while it waits and the going rate rises past it, so
// a claim confirms before the other side's timelock and a refund before the
// other side could still claim.
func (b *bitcoinBTCBackend) spend(leg *LegLock, kind string, asked uint64, build func(uint64) (string, error)) (string, error) {
	var fee uint64
	var vsize int
	txid, err := b.spends.once(b.chain, leg.Funded.TxID, leg.Funded.Vout, kind, func() (string, error) {
		probe, err := build(asked)
		if err != nil {
			return "", err
		}
		// A legacy P2SH spend: no witness, so its vsize is its size. Its size does
		// not depend on the fee (one input, one output), but a DER signature's
		// length varies by up to two bytes from one signing to the next.
		vsize = len(probe)/2 + 2
		rate, _ := b.chain.GoingRate()
		fee = spendFee(asked, rate, vsize, leg.Funded.Amount)
		if fee == asked {
			return probe, nil
		}
		return build(fee)
	})
	if err != nil {
		return "", err
	}
	if fee > 0 { // built now, not a retry of a spend already built
		b.keeper().watchSpend(&pendingSpend{kind: kind, fundTxid: leg.Funded.TxID, vout: leg.Funded.Vout, amount: leg.Funded.Amount,
			txid: txid, fee: fee, vsize: vsize, rebuild: build, cache: &b.spends})
	}
	return txid, nil
}

// locateHTLCOutput finds the (vout, value) of the output paying the script's
// P2SH in a raw Bitcoin tx hex. Used by the in-process LockBTCLeg helper.
func (b *bitcoinBTCBackend) locateHTLCOutput(rawHex string, script []byte) (uint32, uint64, error) {
	wantSPK, err := b.leg.P2SHScriptPubKey(script)
	if err != nil {
		return 0, 0, err
	}
	vout, value, err := findOutputBySPK(rawHex, wantSPK)
	if err != nil {
		return 0, 0, err
	}
	return vout, value, nil
}
