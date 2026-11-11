package xchain

// namedswap.go is swap version 2's settlement against real chains: the
// buyer's payment F from one named coin into a P2WSH hash lock, the seller's
// checks on that coin and that payment, the P2WSH hash lock's claim and
// refund, and the holding output's funding on Sequentia. The lock itself is in
// namedlock.go; the specification is docs/cross-chain-swap-lock.md.

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// BuyerCoin is the coin C a buyer pays from.
type BuyerCoin struct {
	Out  wire.OutPoint
	Sats int64
	Pb   []byte // the key of C's P2WPKH output
}

// PickBuyerCoin chooses, from the bitcoin wallet, one P2WPKH coin of at least
// minSats with at least minConf confirmations whose outpoint is not in skip. It
// prefers the smallest that fits. ErrNoSingleCoin carries what the wallet holds.
func (c *BitcoinChain) PickBuyerCoin(minSats int64, minConf int, skip map[wire.OutPoint]bool) (*BuyerCoin, error) {
	var utxos []struct {
		Txid          string  `json:"txid"`
		Vout          uint32  `json:"vout"`
		Amount        float64 `json:"amount"`
		Address       string  `json:"address"`
		SPK           string  `json:"scriptPubKey"`
		Confirmations int     `json:"confirmations"`
		Spendable     bool    `json:"spendable"`
	}
	if err := c.rpc.Call(&utxos, "listunspent", minConf); err != nil {
		return nil, err
	}
	var best *BuyerCoin
	var total int64
	for _, u := range utxos {
		sats := int64(math.Round(u.Amount * 1e8))
		spk, err := hex.DecodeString(u.SPK)
		if err != nil || len(spk) != 22 || spk[0] != 0x00 || spk[1] != 0x14 || !u.Spendable {
			continue
		}
		total += sats
		h, err := chainhash.NewHashFromStr(u.Txid)
		if err != nil {
			continue
		}
		op := wire.OutPoint{Hash: *h, Index: u.Vout}
		if skip[op] || sats < minSats || (best != nil && sats >= best.Sats) {
			continue
		}
		var info struct {
			PubKey string `json:"pubkey"`
		}
		if err := c.rpc.Call(&info, "getaddressinfo", u.Address); err != nil {
			continue
		}
		pb, err := hex.DecodeString(info.PubKey)
		if err != nil || !bytes.Equal(P2WPKHScript(pb), spk) {
			continue
		}
		best = &BuyerCoin{Out: op, Sats: sats, Pb: pb}
	}
	if best == nil {
		return nil, fmt.Errorf("%w: need one coin of at least %d sats with %d confirmations; the wallet's P2WPKH coins with that age hold %d sats in all",
			ErrNoSingleCoin, minSats, minConf, total)
	}
	return best, nil
}

// ErrNoSingleCoin means no single coin of the size and age exists. The way
// forward is to combine coins into one and trade once it has the confirmations,
// or to trade over Lightning or Arca.
var ErrNoSingleCoin = errors.New("no single bitcoin coin of the size and age this trade needs")

// CheckBuyerCoin is the seller's check of a named coin: unspent, a P2WPKH
// output under Pb, of the amount named. It returns the coin's height.
func (c *BitcoinChain) CheckBuyerCoin(coin BuyerCoin) (int64, error) {
	var out *struct {
		Confirmations int64   `json:"confirmations"`
		Value         float64 `json:"value"`
		SPK           struct {
			Hex string `json:"hex"`
		} `json:"scriptPubKey"`
	}
	if err := c.rpc.Call(&out, "gettxout", coin.Out.Hash.String(), coin.Out.Index, false); err != nil {
		return 0, err
	}
	if out == nil {
		return 0, fmt.Errorf("the buyer's coin %s is spent or unknown", coin.Out)
	}
	if out.SPK.Hex != hex.EncodeToString(P2WPKHScript(coin.Pb)) {
		return 0, fmt.Errorf("the buyer's coin %s is not a P2WPKH output under the key it named", coin.Out)
	}
	if int64(math.Round(out.Value*1e8)) != coin.Sats {
		return 0, fmt.Errorf("the buyer's coin %s holds %.8f, not the %d sats named", coin.Out, out.Value, coin.Sats)
	}
	if out.Confirmations < 1 {
		return 0, fmt.Errorf("the buyer's coin %s is unconfirmed", coin.Out)
	}
	tip, err := c.BlockCount()
	if err != nil {
		return 0, err
	}
	return tip - out.Confirmations + 1, nil
}

// BTCHashLockWitness is the bitcoin hash lock's witness script (the same
// script the P2SH lock used), paid as P2WSH in version 2.
func BTCHashLockWitness(hashH, claimPub, refundPub []byte, locktime uint32) ([]byte, error) {
	return NewHashLockFromHash(hashH).LockScript(claimPub, refundPub, locktime)
}

// BuildPayment builds F, unsigned: coin C into the hash lock's P2WSH output of
// pay sats, the rest less fee to change. Version 2, one input, no
// replaceability signal, nLockTime 0.
func BuildPayment(coin BuyerCoin, lockWS []byte, pay, fee int64, changeSPK []byte) (*wire.MsgTx, error) {
	if pay <= 0 || fee <= 0 || pay+fee > coin.Sats {
		return nil, fmt.Errorf("the coin's %d sats do not cover %d plus a fee of %d", coin.Sats, pay, fee)
	}
	f := wire.NewMsgTx(2)
	f.AddTxIn(&wire.TxIn{PreviousOutPoint: coin.Out, Sequence: 0xfffffffe})
	f.AddTxOut(&wire.TxOut{Value: pay, PkScript: P2WSH(lockWS)})
	if change := coin.Sats - pay - fee; change >= 546 {
		f.AddTxOut(&wire.TxOut{Value: change, PkScript: changeSPK})
	}
	return f, nil
}

// CheckPayment is the seller's check of F before it signs anything: one input,
// the named coin; a standard version; the agreed hash lock paid exactly pay.
// It returns m0.
func CheckPayment(f *wire.MsgTx, coin BuyerCoin, lockWS []byte, pay int64) ([]byte, error) {
	if len(f.TxIn) != 1 || f.TxIn[0].PreviousOutPoint != coin.Out {
		return nil, errors.New("the bitcoin payment must spend exactly the named coin")
	}
	if f.Version < 1 || f.Version > 3 {
		return nil, fmt.Errorf("the bitcoin payment's version %d is not 1, 2 or 3", f.Version)
	}
	paid := false
	for _, o := range f.TxOut {
		if bytes.Equal(o.PkScript, P2WSH(lockWS)) {
			if o.Value != pay || paid {
				return nil, errors.New("the bitcoin payment does not pay the hash lock exactly once, the agreed amount")
			}
			paid = true
		}
	}
	if !paid {
		return nil, errors.New("the bitcoin payment does not pay the agreed hash lock")
	}
	return PaymentM0(f, coin.Pb, coin.Sats)
}

// TxIDOf is a Bitcoin transaction's txid (segwit: known before it is signed).
func TxIDOf(tx *wire.MsgTx) string { return tx.TxHash().String() }

// SignPayment has the bitcoin wallet sign F (SIGHASH_ALL) and checks that it
// signed exactly F. It does not broadcast.
func (c *BitcoinChain) SignPayment(f *wire.MsgTx) (string, error) {
	var buf bytes.Buffer
	if err := f.Serialize(&buf); err != nil {
		return "", err
	}
	var r struct {
		Hex      string `json:"hex"`
		Complete bool   `json:"complete"`
	}
	if err := c.rpc.Call(&r, "signrawtransactionwithwallet", hex.EncodeToString(buf.Bytes()), nil, "ALL"); err != nil {
		return "", err
	}
	if !r.Complete {
		return "", errors.New("the bitcoin wallet could not sign the payment")
	}
	signed, err := ParseBTCTx(r.Hex)
	if err != nil {
		return "", err
	}
	if signed.TxHash() != f.TxHash() {
		return "", errors.New("the wallet signed a different payment")
	}
	return r.Hex, nil
}

// SpendHashLock builds a spend of a P2WSH hash lock output into destSPK: the
// claim with the secret, or (secret nil) the refund at locktime.
func SpendHashLock(op wire.OutPoint, sats int64, lockWS, destSPK []byte, fee int64, key *Key, secret []byte, locktime uint32) (string, error) {
	if fee <= 0 || fee >= sats {
		return "", fmt.Errorf("fee %d must be positive and below the %d locked", fee, sats)
	}
	tx := wire.NewMsgTx(2)
	in := &wire.TxIn{PreviousOutPoint: op, Sequence: 0xfffffffd}
	if secret == nil {
		in.Sequence = 0xfffffffe
		tx.LockTime = locktime
	}
	tx.AddTxIn(in)
	tx.AddTxOut(&wire.TxOut{Value: sats - fee, PkScript: destSPK})
	fetch := txscript.NewCannedPrevOutputFetcher(P2WSH(lockWS), sats)
	h, err := txscript.CalcWitnessSigHash(lockWS, txscript.NewTxSigHashes(tx, fetch), txscript.SigHashAll, tx, 0, sats)
	if err != nil {
		return "", err
	}
	sig := append(key.SignDER(h), byte(txscript.SigHashAll))
	if secret != nil {
		tx.TxIn[0].Witness = wire.TxWitness{sig, secret, {0x01}, lockWS}
	} else {
		tx.TxIn[0].Witness = wire.TxWitness{sig, {}, lockWS}
	}
	var buf bytes.Buffer
	if err := tx.Serialize(&buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf.Bytes()), nil
}

// FindSpendOf looks for a transaction spending op: in the mempool, then in the
// last `depth` blocks. It returns nil when nothing spends it.
func (c *BitcoinChain) FindSpendOf(op wire.OutPoint, depth int) (*wire.MsgTx, error) {
	var mp []struct {
		Spending string `json:"spendingtxid"`
	}
	if err := c.rpc.Call(&mp, "gettxspendingprevout", []map[string]interface{}{{"txid": op.Hash.String(), "vout": op.Index}}); err == nil {
		if len(mp) == 1 && mp[0].Spending != "" {
			raw, err := c.RawTx(mp[0].Spending)
			if err == nil {
				return ParseBTCTx(raw)
			}
		}
	}
	tip, err := c.BlockCount()
	if err != nil {
		return nil, err
	}
	for h := tip; h > tip-int64(depth) && h > 0; h-- {
		var bh string
		if err := c.rpc.Call(&bh, "getblockhash", h); err != nil {
			return nil, err
		}
		var raw string
		if err := c.rpc.Call(&raw, "getblock", bh, 0); err != nil {
			return nil, err
		}
		b, err := hex.DecodeString(raw)
		if err != nil {
			return nil, err
		}
		var blk wire.MsgBlock
		if err := blk.Deserialize(bytes.NewReader(b)); err != nil {
			return nil, err
		}
		for _, tx := range blk.Transactions {
			for _, in := range tx.TxIn {
				if in.PreviousOutPoint == op {
					return tx, nil
				}
			}
		}
	}
	return nil, nil
}

// FundHolding pays amountAtoms of asset into the holding output from the
// Sequentia wallet, not replaceable, and returns the output.
func (c *Chain) FundHolding(holdingWS []byte, amountAtoms uint64, assetLabel, assetID string) (NamedSpend, error) {
	var dec struct {
		Segwit struct {
			Address string `json:"address"`
		} `json:"segwit"`
	}
	if err := c.rpc.Call(&dec, "decodescript", hex.EncodeToString(holdingWS)); err != nil {
		return NamedSpend{}, err
	}
	var txid string
	named := map[string]interface{}{"address": dec.Segwit.Address,
		"amount":     fmt.Sprintf("%d.%08d", amountAtoms/100_000_000, amountAtoms%100_000_000),
		"assetlabel": assetLabel, "fee_asset_label": assetLabel, "replaceable": false}
	if err := c.rpc.CallNamed(&txid, "sendtoaddress", named); err != nil {
		return NamedSpend{}, err
	}
	return c.FindNamedOutput(txid, P2WSH(holdingWS), assetID)
}

// FindNamedOutput returns the explicit output of txid paying spk in assetID.
func (c *Chain) FindNamedOutput(txid string, spk []byte, assetID string) (NamedSpend, error) {
	var r struct {
		Vout []struct {
			N     uint32  `json:"n"`
			Value float64 `json:"value"`
			Asset string  `json:"asset"`
			SPK   struct {
				Hex string `json:"hex"`
			} `json:"scriptPubKey"`
		} `json:"vout"`
	}
	if err := c.rpc.Call(&r, "getrawtransaction", txid, true); err != nil {
		return NamedSpend{}, err
	}
	for _, v := range r.Vout {
		if v.SPK.Hex == hex.EncodeToString(spk) && strings.EqualFold(v.Asset, assetID) {
			return NamedSpend{TxID: txid, Vout: v.N, Amount: uint64(math.Round(v.Value * 1e8)), AssetID: assetID}, nil
		}
	}
	return NamedSpend{}, fmt.Errorf("transaction %s has no explicit %s output paying %x", txid, assetID, spk)
}

// SeqTxReplaceable reports whether a Sequentia transaction signals
// replaceability (any input below 0xfffffffe).
func (c *Chain) SeqTxReplaceable(txid string) (bool, error) {
	var r struct {
		Vin []struct {
			Sequence uint32 `json:"sequence"`
		} `json:"vin"`
	}
	if err := c.rpc.Call(&r, "getrawtransaction", txid, true); err != nil {
		return false, err
	}
	for _, in := range r.Vin {
		if in.Sequence < 0xfffffffe {
			return true, nil
		}
	}
	return false, nil
}
