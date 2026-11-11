package xchain

// Regtest proof of the named lock (docs/cross-chain-swap-lock.md) against a real
// bitcoind and a Sequentia node anchored to it (-validateanchor=1 -par=1, so a
// refused block names its script failure). Every negative case is forced into a
// block with generateblock, and the test asserts the reason the block was refused.
//
//	NAMEDLOCK_DIR=/dev/shm/$USER-namedlock bash testdata/start-regtest-bitcoind.sh up
//	SEQDEX_NAMEDLOCK_ENV=$NAMEDLOCK_DIR/env go test ./pkg/xchain -run '^TestNamedLockRegtest' -v -count=1
//	NAMEDLOCK_DIR=... bash testdata/start-regtest-bitcoind.sh stop
//
// The scenarios are the proof review's runs on the prototype lock, carried over
// to this one: the forced negatives of every branch, the buyer's attack (a Bitcoin
// reorganization and a double-spend of the payment, with the seller early and
// late), the seller's attack (a reorganization that keeps the payment), a coin
// younger than the reorganization, and the attacks this lock adds checks for
// (the seller's claim signature reused in the cancel branch, a re-fee'd claim).

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/vulpemventures/go-elements/transaction"
)

type nlEnv struct {
	btc, seq       *RPC
	asset          string
	minerB, minerS string
}

func nlLoad(t *testing.T) *nlEnv {
	path := os.Getenv("SEQDEX_NAMEDLOCK_ENV")
	if path == "" {
		t.Skip("SEQDEX_NAMEDLOCK_ENV not set (see testdata/start-regtest-bitcoind.sh)")
	}
	raw, err := os.ReadFile(path)
	nlMust(t, err)
	kv := map[string]string{}
	for _, l := range strings.Split(string(raw), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok {
			kv[k] = v
		}
	}
	rpc := func(s string) *RPC {
		p := strings.SplitN(s, ":", 4)
		port, _ := strconv.Atoi(p[1])
		return NewRPC(p[0], port, p[2], p[3])
	}
	e := &nlEnv{btc: rpc(kv["NAMEDLOCK_BTC_RPC"]), seq: rpc(kv["NAMEDLOCK_SEQ_RPC"]), asset: kv["NAMEDLOCK_ASSET"]}
	nlMust(t, e.btc.WithWallet("w").Call(&e.minerB, "getnewaddress"))
	nlMust(t, e.seq.WithWallet("w").Call(&e.minerS, "getnewaddress"))
	return e
}

func nlMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (e *nlEnv) mineBtc(t *testing.T, n int) []string {
	var h []string
	nlMust(t, e.btc.Call(&h, "generatetoaddress", n, e.minerB))
	return h
}

// mineBtcFresh mines to a new address, so a block mined after invalidateblock
// is not the invalidated block again (same template, same address, same time).
func (e *nlEnv) mineBtcFresh(t *testing.T, n int) []string {
	var a string
	nlMust(t, e.btc.WithWallet("w").Call(&a, "getnewaddress"))
	var h []string
	nlMust(t, e.btc.Call(&h, "generatetoaddress", n, a))
	return h
}

func (e *nlEnv) mineSeq(t *testing.T, n int) []string {
	var h []string
	nlMust(t, e.seq.Call(&h, "generatetoaddress", n, e.minerS))
	return h
}

type nlHdr struct {
	Height        int64 `json:"height"`
	AnchorHeight  int64 `json:"anchorheight"`
	Confirmations int64 `json:"confirmations"`
}

func (e *nlEnv) hdr(t *testing.T, h string) nlHdr {
	var x nlHdr
	nlMust(t, e.seq.Call(&x, "getblockheader", h))
	return x
}

func (e *nlEnv) btcTip(t *testing.T) int64 {
	var n int64
	nlMust(t, e.btc.Call(&n, "getblockcount"))
	return n
}

func (e *nlEnv) seqTip(t *testing.T) int64 {
	var n int64
	nlMust(t, e.seq.Call(&n, "getblockcount"))
	return n
}

// waitAnchor mines Sequentia blocks until one anchors at h or above.
func (e *nlEnv) waitAnchor(t *testing.T, h int64) {
	for i := 0; i < 120; i++ {
		b := e.mineSeq(t, 1)[0]
		if e.hdr(t, b).AnchorHeight >= h {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("no Sequentia block anchored at %d", h)
}

func (e *nlEnv) waitDisconnected(t *testing.T, block string) {
	for i := 0; i < 120; i++ {
		if e.hdr(t, block).Confirmations < 0 {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("block %s was not disconnected by the anchor watcher", block)
}

// force has a producer put txs into a block, ignoring its mempool, and asserts
// the block is refused for the named reason.
func (e *nlEnv) force(t *testing.T, label string, want string, hexes ...string) {
	t.Helper()
	var r interface{}
	err := e.seq.Call(&r, "generateblock", e.minerS, hexes)
	if err == nil {
		t.Fatalf("FORCED %s: the block was ACCEPTED (%v)", label, r)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("FORCED %s: refused for the wrong reason: %v (want %q)", label, err, want)
	}
	t.Logf("forced %s: refused: %v", label, err)
}

func (e *nlEnv) send(t *testing.T, hexTx string) (string, error) {
	var id string
	err := e.seq.Call(&id, "sendrawtransaction", hexTx)
	return id, err
}

func (e *nlEnv) inBlock(t *testing.T, block, txid string) bool {
	var b struct {
		Tx []string `json:"tx"`
	}
	nlMust(t, e.seq.Call(&b, "getblock", block))
	for _, x := range b.Tx {
		if x == txid {
			return true
		}
	}
	return false
}

// confirmed reports whether txid is in a block of the active chain.
func (e *nlEnv) confirmed(t *testing.T, txid string) bool {
	var r struct {
		Confirmations int64 `json:"confirmations"`
	}
	if err := e.seq.Call(&r, "getrawtransaction", txid, true); err != nil {
		return false
	}
	return r.Confirmations > 0
}

func (e *nlEnv) seqSPK(t *testing.T, wallet string) []byte {
	var a string
	nlMust(t, e.seq.WithWallet(wallet).Call(&a, "getnewaddress"))
	var info struct {
		SPK string `json:"scriptPubKey"`
	}
	nlMust(t, e.seq.WithWallet(wallet).Call(&info, "getaddressinfo", a))
	b, _ := hex.DecodeString(info.SPK)
	return b
}

func (e *nlEnv) btcSPK(t *testing.T, wallet string) []byte {
	var a string
	nlMust(t, e.btc.WithWallet(wallet).Call(&a, "getnewaddress", "", "bech32"))
	var info struct {
		SPK string `json:"scriptPubKey"`
	}
	nlMust(t, e.btc.WithWallet(wallet).Call(&info, "getaddressinfo", a))
	b, _ := hex.DecodeString(info.SPK)
	return b
}

// outputOf finds the output of txid paying spk on Sequentia.
func (e *nlEnv) outputOf(t *testing.T, txid string, spk []byte) (uint32, uint64) {
	var r struct {
		Vout []struct {
			N     uint32  `json:"n"`
			Value float64 `json:"value"`
			SPK   struct {
				Hex string `json:"hex"`
			} `json:"scriptPubKey"`
		} `json:"vout"`
	}
	nlMust(t, e.seq.Call(&r, "getrawtransaction", txid, true))
	for _, v := range r.Vout {
		if v.SPK.Hex == hex.EncodeToString(spk) {
			return v.N, uint64(v.Value*1e8 + 0.5)
		}
	}
	t.Fatalf("no output of %s pays %x", txid, spk)
	return 0, 0
}

// --- one swap's state -------------------------------------------------------------

type nlSwap struct {
	secret, hashH []byte
	pb            *Key // the buyer's bitcoin coin key
	bh, bc, bo    *Key
	sh, sc, sx    *Key
	c             wire.OutPoint
	cHeight       int64
	f             *wire.MsgTx // signed
	fID           string
	fHeight       int64
	m0            []byte
	terms         NamedLockTerms
	holding, lock []byte
	hold, lockOut NamedSpend
	lockHex       string
	lockID        string
	lockBlock     string
	claim         *transaction.Transaction
	claimSc       []byte
	claimHex      string
	cancel        *transaction.Transaction
	cancelSx      []byte
	sellerSPK     []byte
}

const (
	nlCoinSats  = 200_000_000
	nlLockAtoms = 500 * 100_000_000
	nlLockFee   = 10_000
	nlClaimFee  = 3_000
	nlCancelFee = 20_000
)

func nlKey(t *testing.T) *Key {
	k, err := NewKey()
	nlMust(t, err)
	return k
}

// btcSign signs input 0 of tx spending the P2WPKH coin of key k.
func btcSign(t *testing.T, tx *wire.MsgTx, k *Key, amount int64) {
	fetch := txscript.NewCannedPrevOutputFetcher(P2WPKHScript(k.PubKey()), amount)
	w, err := txscript.WitnessSignature(tx, txscript.NewTxSigHashes(tx, fetch), 0, amount, P2WPKHScript(k.PubKey()),
		txscript.SigHashAll, k.priv, true)
	nlMust(t, err)
	tx.TxIn[0].Witness = w
}

func btcHex(t *testing.T, tx *wire.MsgTx) string {
	var b bytes.Buffer
	nlMust(t, tx.Serialize(&b))
	return hex.EncodeToString(b.Bytes())
}

// setup runs the swap to a mined lock: the buyer's coin aged coinAge blocks, the
// holding output confirmed before the payment, the payment confirmed, the anchor
// past it, the lock completed and mined. stopBeforeLock leaves the lock unsent.
func (e *nlEnv) setup(t *testing.T, coinAge int, stopBeforeLock bool) *nlSwap {
	s := &nlSwap{pb: nlKey(t), bh: nlKey(t), bc: nlKey(t), bo: nlKey(t), sh: nlKey(t), sc: nlKey(t), sx: nlKey(t)}
	s.secret = make([]byte, 32)
	_, _ = rand.Read(s.secret)
	h := sha256.Sum256(s.secret)
	s.hashH = h[:]

	// The buyer's coin C, a P2WPKH output under Pb.
	addr, err := btcutil.NewAddressWitnessPubKeyHash(btcutil.Hash160(s.pb.PubKey()), &chaincfg.RegressionNetParams)
	nlMust(t, err)
	var cTxid string
	nlMust(t, e.btc.WithWallet("w").Call(&cTxid, "sendtoaddress", addr.EncodeAddress(), 2))
	e.mineBtc(t, 1)
	s.cHeight = e.btcTip(t)
	var ctx struct {
		Vout []struct {
			N   uint32 `json:"n"`
			SPK struct {
				Hex string `json:"hex"`
			} `json:"scriptPubKey"`
		} `json:"vout"`
	}
	nlMust(t, e.btc.Call(&ctx, "getrawtransaction", cTxid, true))
	ch, _ := chainhash.NewHashFromStr(cTxid)
	for _, v := range ctx.Vout {
		if v.SPK.Hex == hex.EncodeToString(P2WPKHScript(s.pb.PubKey())) {
			s.c = wire.OutPoint{Hash: *ch, Index: v.N}
		}
	}

	// The holding output, funded by the seller, not replaceable, confirmed first.
	th := uint32(e.seqTip(t) + 1000)
	s.holding, err = HoldingScript(s.sh.PubKey(), s.bh.PubKey(), th)
	nlMust(t, err)
	var dec struct {
		Segwit struct {
			Address string `json:"address"`
		} `json:"segwit"`
	}
	nlMust(t, e.seq.Call(&dec, "decodescript", hex.EncodeToString(s.holding)))
	var hfID string
	nlMust(t, e.seq.WithWallet("maker").CallNamed(&hfID, "sendtoaddress", map[string]interface{}{
		"address": dec.Segwit.Address, "amount": fmt.Sprintf("%d.%08d", uint64(nlLockAtoms+nlLockFee)/100_000_000, uint64(nlLockAtoms+nlLockFee)%100_000_000),
		"assetlabel": e.asset, "fee_asset_label": e.asset, "replaceable": false}))
	hv, ha := e.outputOf(t, hfID, P2WSH(s.holding))
	s.hold = NamedSpend{TxID: hfID, Vout: hv, Amount: ha, AssetID: e.asset}
	e.mineSeq(t, 1)
	if coinAge > 1 {
		e.mineBtc(t, coinAge-1)
	}

	// F: C's only spend the buyer ever signs, unsigned so far.
	s.f = wire.NewMsgTx(2)
	s.f.AddTxIn(&wire.TxIn{PreviousOutPoint: s.c, Sequence: 0xffffffff})
	s.f.AddTxOut(&wire.TxOut{Value: 100_000_000, PkScript: e.btcSPK(t, "maker")})
	s.f.AddTxOut(&wire.TxOut{Value: 99_990_000, PkScript: e.btcSPK(t, "taker")})
	s.m0, err = PaymentM0(s.f, s.pb.PubKey(), nlCoinSats)
	nlMust(t, err)

	// The lock, its fixed claim, and the seller's pre-signed cancel.
	s.terms = NamedLockTerms{HashH: s.hashH, M0: s.m0, Pb: s.pb.PubKey(), C: s.c, CSats: nlCoinSats, Bc: s.bc.PubKey(), Sc: s.sc.PubKey(),
		Sx: s.sx.PubKey(), T: uint32(e.seqTip(t) + 200), K2: DefaultLateK2}
	s.lock, err = NamedLockScript(s.terms)
	nlMust(t, err)
	ltx, err := BuildLockTx(s.hold, s.lock, nlLockAtoms)
	nlMust(t, err)
	s.lockID = ltx.TxHash().String()
	s.lockOut = NamedSpend{TxID: s.lockID, Vout: 0, Amount: nlLockAtoms, AssetID: e.asset}
	sigBh, err := SignWitness(ltx, s.holding, s.hold.Amount, s.bh)
	nlMust(t, err)
	s.claim, err = BuildClaimTx(s.lockOut, s.bo.PubKey(), nlClaimFee)
	nlMust(t, err)
	s.claimSc, err = SignWitness(s.claim, s.lock, nlLockAtoms, s.sc)
	nlMust(t, err)
	s.sellerSPK = e.seqSPK(t, "maker")
	s.cancel, err = BuildCancelTx(s.lockOut, s.sellerSPK, nlCancelFee)
	nlMust(t, err)
	s.cancelSx, err = SignWitness(s.cancel, s.lock, nlLockAtoms, s.sx)
	nlMust(t, err)

	// The buyer signs F and broadcasts it; it confirms; the anchor passes it.
	btcSign(t, s.f, s.pb, nlCoinSats)
	nlMust(t, e.btc.Call(&s.fID, "sendrawtransaction", btcHex(t, s.f)))
	e.mineBtc(t, 1)
	s.fHeight = e.btcTip(t)
	e.mineBtc(t, 1)
	e.waitAnchor(t, s.fHeight+1)
	t.Logf("C %s at %d; holding %s:%d; F %s at %d; m0 %x", s.c, s.cHeight, hfID, hv, s.fID, s.fHeight, s.m0)

	sigSh, err := SignWitness(ltx, s.holding, s.hold.Amount, s.sh)
	nlMust(t, err)
	s.lockHex, err = FinalizeLockTx(ltx, s.holding, sigSh, sigBh)
	nlMust(t, err)
	if stopBeforeLock {
		return s
	}
	id, err := e.send(t, s.lockHex)
	nlMust(t, err)
	if id != s.lockID {
		t.Fatalf("lock txid %s != the %s every pre-signature names", id, s.lockID)
	}
	s.lockBlock = e.mineSeq(t, 1)[0]
	hd := e.hdr(t, s.lockBlock)
	t.Logf("lock %s mined in %s height %d anchor %d", s.lockID, s.lockBlock[:16], hd.Height, hd.AnchorHeight)
	return s
}

func (s *nlSwap) claimTx(t *testing.T) string {
	if s.claimHex != "" {
		return s.claimHex
	}
	tx := s.claim.Copy()
	sigBc, err := SignWitness(tx, s.lock, nlLockAtoms, s.bc)
	nlMust(t, err)
	s.claimHex, err = FinalizeClaimTx(tx, s.lock, sigBc, s.claimSc, s.secret)
	nlMust(t, err)
	return s.claimHex
}

func (s *nlSwap) cancelTx(t *testing.T, p CancelProofPieces) string {
	h, err := FinalizeCancelTx(s.cancel.Copy(), s.lock, s.cancelSx, p)
	nlMust(t, err)
	return h
}

// fPieces is F's own signature and preimage, cut as a cancel carries them.
func (s *nlSwap) fPieces(t *testing.T) CancelProofPieces {
	w := s.f.TxIn[0].Witness[0]
	pre, err := BIP143Preimage(s.f, 0, s.pb.PubKey(), nlCoinSats, txscript.SigHashAll)
	nlMust(t, err)
	return PiecesOf(w[:len(w)-1], pre)
}

// piecesOf cuts any 182-byte message as a cancel carries it, signed by k.
func piecesOf(k *Key, msg []byte) CancelProofPieces {
	h := chainhash.DoubleHashB(msg)
	return PiecesOf(k.SignDER(h), msg)
}

// spendOf signs a spend of outpoint op (a P2WPKH coin of k worth sats) with hash type ht.
func spendOf(t *testing.T, op wire.OutPoint, k *Key, sats int64, spk []byte, ht txscript.SigHashType) *wire.MsgTx {
	return spendOfV(t, op, k, sats, spk, ht, 2)
}

func spendOfV(t *testing.T, op wire.OutPoint, k *Key, sats int64, spk []byte, ht txscript.SigHashType, version int32) *wire.MsgTx {
	d := wire.NewMsgTx(version)
	d.AddTxIn(&wire.TxIn{PreviousOutPoint: op, Sequence: 0xfffffffd})
	d.AddTxOut(&wire.TxOut{Value: sats - 20_000, PkScript: spk})
	fetch := txscript.NewCannedPrevOutputFetcher(P2WPKHScript(k.PubKey()), sats)
	w, err := txscript.WitnessSignature(d, txscript.NewTxSigHashes(d, fetch), 0, sats, P2WPKHScript(k.PubKey()), ht, k.priv, true)
	nlMust(t, err)
	d.TxIn[0].Witness = w
	return d
}

// reorgDoubleSpend reorganizes Bitcoin from height fork and confirms the buyer's
// spend of C to itself in place of F.
func (e *nlEnv) reorgDoubleSpend(t *testing.T, s *nlSwap, fork int64) *wire.MsgTx {
	return e.reorgDoubleSpendV(t, s, fork, 2)
}

// reorgDoubleSpendV is reorgDoubleSpend with a chosen version; a non-standard
// one is mined directly, as a pool taking it for a fee would.
func (e *nlEnv) reorgDoubleSpendV(t *testing.T, s *nlSwap, fork int64, version int32) *wire.MsgTx {
	tip := e.btcTip(t)
	var bh string
	nlMust(t, e.btc.Call(&bh, "getblockhash", fork))
	nlMust(t, e.btc.Call(nil, "invalidateblock", bh))
	d := wire.NewMsgTx(version)
	d.AddTxIn(&wire.TxIn{PreviousOutPoint: s.c, Sequence: 0xffffffff})
	d.AddTxOut(&wire.TxOut{Value: 199_900_000, PkScript: e.btcSPK(t, "taker")}) // outbids F under full RBF
	btcSign(t, d, s.pb, nlCoinSats)
	did := d.TxHash().String()
	if version >= 1 && version <= 3 {
		nlMust(t, e.btc.Call(&did, "sendrawtransaction", btcHex(t, d)))
		e.mineBtc(t, int(tip-fork+2))
	} else {
		var r interface{}
		nlMust(t, e.btc.Call(&r, "generateblock", e.minerB, []string{btcHex(t, d)}))
		e.mineBtc(t, int(tip-fork+1))
	}
	t.Logf("Bitcoin reorganized from %d (%d blocks replaced); C double-spent by %s", fork, tip-fork+1, did)
	return d
}

func TestNamedLockRegtest(t *testing.T) {
	e := nlLoad(t)

	t.Run("paths", func(t *testing.T) {
		s := e.setup(t, DefaultCoinAge, true)

		// The holding output: the seller alone cannot move it before Th.
		bogus := nlKey(t)
		alt, err := BuildCancelTx(s.hold, s.sellerSPK, 30_000)
		nlMust(t, err)
		a1, _ := SignWitness(alt, s.holding, s.hold.Amount, s.sh)
		a2, _ := SignWitness(alt, s.holding, s.hold.Amount, bogus)
		altHex, _ := FinalizeLockTx(alt, s.holding, a1, a2)
		e.force(t, "the seller moves the holding output with a stranger's second signature", "finished with a false/empty top stack element", altHex)
		thTx, err := BuildRefundTx(s.hold, s.sellerSPK, 30_000, uint32(e.seqTip(t)+1000))
		nlMust(t, err)
		thSig, _ := SignWitness(thTx, s.holding, s.hold.Amount, s.sh)
		thHex, _ := setWitness(thTx, thSig, []byte{}, s.holding)
		e.force(t, "the seller's lone return of the holding output before Th", "bad-txns-nonfinal", thHex)

		// The claim cannot share a block with its lock.
		e.force(t, "the lock and its claim in one block", "bad-txns-nonfinal", s.lockHex, s.claimTx(t))
		_, err = e.send(t, s.lockHex)
		nlMust(t, err)
		s.lockBlock = e.mineSeq(t, 1)[0]

		// The claim with nSequence 0 (both keys signing it).
		c0 := s.claim.Copy()
		c0.Inputs[0].Sequence = 0
		sc0, _ := SignWitness(c0, s.lock, nlLockAtoms, s.sc)
		bc0, _ := SignWitness(c0, s.lock, nlLockAtoms, s.bc)
		c0Hex, _ := FinalizeClaimTx(c0, s.lock, bc0, sc0, s.secret)
		e.force(t, "a claim with nSequence 0", "Locktime requirement not satisfied", c0Hex)

		// A claim at a higher fee than the one the seller signed.
		hi, err := BuildClaimTx(s.lockOut, s.bo.PubKey(), 60_000)
		nlMust(t, err)
		bhi, _ := SignWitness(hi, s.lock, nlLockAtoms, s.bc)
		hiHex, _ := FinalizeClaimTx(hi, s.lock, bhi, s.claimSc, s.secret)
		e.force(t, "a claim re-fee'd by the buyer over the seller's fixed-claim signature", "Script failed an OP_CHECKSIGVERIFY operation", hiHex)

		// The seller's claim signature reused in the cancel branch, with a real second spend of C.
		dsAll := spendOf(t, s.c, s.pb, nlCoinSats, e.btcSPK(t, "taker"), txscript.SigHashAll)
		pAll, err := CancelProof(dsAll, s.c, s.pb.PubKey(), nlCoinSats, s.m0)
		nlMust(t, err)
		reuse, _ := FinalizeCancelTx(s.claim.Copy(), s.lock, s.claimSc, pAll)
		e.force(t, "the fixed claim spent through the cancel branch (the seller's Sc signature, a real Pb proof)", "finished with a false/empty top stack element", reuse)

		// Not a proof: F's own signature and its high-S twin.
		fp := s.fPieces(t)
		e.force(t, "a cancel with F's own signature", "Script failed an OP_VERIFY operation", s.cancelTx(t, fp))
		fh := fp
		fh.Sig = nlHighS(fp.Sig)
		e.force(t, "a cancel with F's signature made high-S", "Script failed an OP_VERIFY operation", s.cancelTx(t, fh))

		// Not a proof: a Sequentia signature by the same key (the wallet's shared address).
		seqTx, _ := BuildCancelTx(s.lockOut, s.sellerSPK, 1_000)
		seqH, _ := WitnessSighash(seqTx, P2WPKHScript(s.pb.PubKey()), nlLockAtoms)
		ps := fp
		ps.Sig = s.pb.SignDER(seqH[:])
		ps.Tail = append(append([]byte{}, fp.Tail[:40]...), 0x02, 0, 0, 0)
		e.force(t, "a cancel proven by Pb's signature over a Sequentia transaction", "Script failed an OP_CHECKSIGVERIFY operation", s.cancelTx(t, ps))

		// Not a proof: a signed message crafted to carry C's outpoint, script code and amount.
		body := make([]byte, 156)
		copy(body[42:], coinMiddle(s.c, s.pb.PubKey(), nlCoinSats))
		crafted := append(append(append([]byte{}, SignedMessagePrefix...), 156), body...)
		if len(crafted) != 182 || !bytes.Equal(crafted[68:138], coinMiddle(s.c, s.pb.PubKey(), nlCoinSats)) {
			t.Fatalf("crafted message is %d bytes", len(crafted))
		}
		e.force(t, "a cancel proven by a signed message crafted around C", "Script failed an OP_VERIFY operation", s.cancelTx(t, piecesOf(s.pb, crafted)))

		// Not a proof: a spend of another bitcoin coin at the same address.
		addr, _ := btcutil.NewAddressWitnessPubKeyHash(btcutil.Hash160(s.pb.PubKey()), &chaincfg.RegressionNetParams)
		var c2id string
		nlMust(t, e.btc.WithWallet("w").Call(&c2id, "sendtoaddress", addr.EncodeAddress(), 1))
		c2h, _ := chainhash.NewHashFromStr(c2id)
		var c2 wire.OutPoint
		for n := uint32(0); n < 2; n++ {
			var o map[string]interface{}
			if e.btc.Call(&o, "gettxout", c2id, n, true) == nil && o != nil {
				if sp, _ := o["scriptPubKey"].(map[string]interface{}); sp["hex"] == hex.EncodeToString(P2WPKHScript(s.pb.PubKey())) {
					c2 = wire.OutPoint{Hash: *c2h, Index: n}
				}
			}
		}
		d2 := spendOf(t, c2, s.pb, 100_000_000, e.btcSPK(t, "taker"), txscript.SigHashAll)
		var d2id string
		nlMust(t, e.btc.Call(&d2id, "sendrawtransaction", btcHex(t, d2)))
		pre2, _ := BIP143Preimage(d2, 0, s.pb.PubKey(), 100_000_000, txscript.SigHashAll)
		w2 := d2.TxIn[0].Witness[0]
		p2 := PiecesOf(w2[:len(w2)-1], pre2)
		if _, err := CancelProof(d2, s.c, s.pb.PubKey(), nlCoinSats, s.m0); err == nil {
			t.Fatal("CancelProof took a spend of another coin")
		}
		e.force(t, "a cancel proven by a real spend of another coin at the same address", "Script failed an OP_CHECKSIGVERIFY operation", s.cancelTx(t, p2))

		// A proof: a second spend of C under every hash type (script checked by the mempool's test).
		for _, ht := range []txscript.SigHashType{txscript.SigHashAll, txscript.SigHashNone, txscript.SigHashSingle,
			txscript.SigHashAll | txscript.SigHashAnyOneCanPay, txscript.SigHashNone | txscript.SigHashAnyOneCanPay,
			txscript.SigHashSingle | txscript.SigHashAnyOneCanPay} {
			ds := spendOf(t, s.c, s.pb, nlCoinSats, e.btcSPK(t, "taker"), ht)
			p, err := CancelProof(ds, s.c, s.pb.PubKey(), nlCoinSats, s.m0)
			nlMust(t, err)
			var res []struct {
				Allowed bool   `json:"allowed"`
				Reason  string `json:"reject-reason"`
			}
			nlMust(t, e.seq.Call(&res, "testmempoolaccept", []string{s.cancelTx(t, p)}))
			if !res[0].Allowed {
				t.Fatalf("a real second spend of C under hash type %#x was refused as a proof: %s", ht, res[0].Reason)
			}
			t.Logf("a second spend of C under hash type %#x: the cancel is valid", ht)
		}
		// A proof whatever the double-spend's version: 256, and the one whose bytes are 18 42 69 74.
		for _, v := range []int32{256, 0x74694218} {
			ds := spendOfV(t, s.c, s.pb, nlCoinSats, e.btcSPK(t, "taker"), txscript.SigHashAll, v)
			p, err := CancelProof(ds, s.c, s.pb.PubKey(), nlCoinSats, s.m0)
			nlMust(t, err)
			var res []struct {
				Allowed bool   `json:"allowed"`
				Reason  string `json:"reject-reason"`
			}
			nlMust(t, e.seq.Call(&res, "testmempoolaccept", []string{s.cancelTx(t, p)}))
			if !res[0].Allowed {
				t.Fatalf("a second spend of C with version %#x was refused as a proof: %s", v, res[0].Reason)
			}
			t.Logf("a second spend of C with version %#x (bytes %x): the cancel is valid", v, p.Version)
		}

		// The late claim before k2, and with a shorter sequence.
		dest := e.seqSPK(t, "taker")
		lc, _ := BuildLateClaimTx(s.lockOut, dest, 5_000, DefaultLateK2)
		lcs, _ := SignWitness(lc, s.lock, nlLockAtoms, s.bc)
		lcHex, _ := FinalizeLateClaimTx(lc, s.lock, lcs, s.secret)
		e.force(t, "the late claim one block after the lock", "bad-txns-nonfinal", lcHex)
		lc1, _ := BuildLateClaimTx(s.lockOut, dest, 5_000, 1)
		lcs1, _ := SignWitness(lc1, s.lock, nlLockAtoms, s.bc)
		lc1Hex, _ := FinalizeLateClaimTx(lc1, s.lock, lcs1, s.secret)
		e.force(t, "the late claim with nSequence 1", "Locktime requirement not satisfied", lc1Hex)

		// The refund before T.
		rf, _ := BuildRefundTx(s.lockOut, s.sellerSPK, 5_000, s.terms.T)
		rfs, _ := SignWitness(rf, s.lock, nlLockAtoms, s.sx)
		rfHex, _ := FinalizeRefundTx(rf, s.lock, rfs)
		e.force(t, "the refund before T", "bad-txns-nonfinal", rfHex)

		// The happy path: the fixed claim in the next block, then the claim output moved on.
		cid, err := e.send(t, s.claimTx(t))
		nlMust(t, err)
		var ent struct {
			RBF bool `json:"bip125-replaceable"`
		}
		nlMust(t, e.seq.Call(&ent, "getmempoolentry", cid))
		b := e.mineSeq(t, 1)[0]
		if !e.inBlock(t, b, cid) {
			t.Fatalf("the fixed claim %s was not mined in the block after the lock", cid)
		}
		t.Logf("claim %s mined in the block after the lock (replaceable in the mempool: %v)", cid, ent.RBF)
		cs, _ := ClaimOutScript(s.bo.PubKey())
		out := NamedSpend{TxID: cid, Vout: 0, Amount: nlLockAtoms - nlClaimFee, AssetID: e.asset}
		mv, _ := BuildClaimOutSpend(out, dest, 3_000)
		mvs, _ := SignWitness(mv, cs, out.Amount, s.bo)
		mvHex, _ := FinalizeClaimOutSpend(mv, cs, mvs)
		mid, err := e.send(t, mvHex)
		nlMust(t, err)
		if !e.inBlock(t, e.mineSeq(t, 1)[0], mid) {
			t.Fatal("the claim output's move was not mined")
		}
	})

	t.Run("buyer_double_spends_seller_early", func(t *testing.T) {
		s := e.setup(t, DefaultCoinAge, false)
		cid, err := e.send(t, s.claimTx(t))
		nlMust(t, err)
		e.mineSeq(t, 1)
		d := e.reorgDoubleSpend(t, s, s.fHeight)
		e.waitDisconnected(t, s.lockBlock)
		e.force(t, "the returned lock and the buyer's claim in one block", "bad-txns-nonfinal", s.lockHex, s.claimTx(t))
		proof, err := CancelProof(d, s.c, s.pb.PubKey(), nlCoinSats, s.m0)
		nlMust(t, err)
		xid, err := e.send(t, s.cancelTx(t, proof))
		nlMust(t, err)
		b := e.mineSeq(t, 1)[0]
		if !e.inBlock(t, b, s.lockID) || !e.inBlock(t, b, xid) {
			t.Fatalf("the next block does not carry the lock and the cancel")
		}
		e.force(t, "the buyer's claim after the cancel", "bad-txns-inputs-missingorspent", s.claimTx(t))
		e.mineSeq(t, 2)
		if e.confirmed(t, cid) || !e.confirmed(t, xid) {
			t.Fatalf("claim confirmed=%v cancel confirmed=%v", e.confirmed(t, cid), e.confirmed(t, xid))
		}
		t.Logf("RESULT: the asset went back to the seller (%s); the buyer's claim is gone", xid)
	})

	t.Run("buyer_double_spends_seller_late", func(t *testing.T) {
		s := e.setup(t, DefaultCoinAge, false)
		nlMust(t, func() error { _, err := e.send(t, s.claimTx(t)); return err }())
		e.mineSeq(t, 1)
		d := e.reorgDoubleSpend(t, s, s.fHeight)
		e.waitDisconnected(t, s.lockBlock)
		b := e.mineSeq(t, 1)[0]
		if !e.inBlock(t, b, s.lockID) {
			t.Fatal("the lock was not mined again")
		}
		cid, err := e.send(t, s.claimTx(t))
		nlMust(t, err)
		proof, err := CancelProof(d, s.c, s.pb.PubKey(), nlCoinSats, s.m0)
		nlMust(t, err)
		xid, err := e.send(t, s.cancelTx(t, proof))
		nlMust(t, err)
		var ent interface{}
		if err := e.seq.Call(&ent, "getmempoolentry", cid); err == nil {
			t.Fatal("the cancel did not replace the claim waiting in the mempool")
		}
		// The buyer cannot answer: its claim is fixed, and a re-fee'd one fails the seller's signature.
		if _, err := e.send(t, s.claimTx(t)); err == nil {
			t.Fatal("the buyer's claim re-entered the mempool over the cancel")
		} else {
			t.Logf("the buyer's claim, sent again: %v", err)
		}
		if !e.inBlock(t, e.mineSeq(t, 1)[0], xid) {
			t.Fatal("the cancel was not mined")
		}
		t.Logf("RESULT: the late cancel (fee %d) replaced the fixed claim (fee %d) and was mined", nlCancelFee, nlClaimFee)
	})

	t.Run("buyer_double_spends_before_claiming_version_256_mined_by_a_pool", func(t *testing.T) {
		s := e.setup(t, DefaultCoinAge, false)
		d := e.reorgDoubleSpendV(t, s, s.fHeight, 256)
		e.waitDisconnected(t, s.lockBlock)
		proof, err := CancelProof(d, s.c, s.pb.PubKey(), nlCoinSats, s.m0)
		nlMust(t, err)
		xid, err := e.send(t, s.cancelTx(t, proof))
		nlMust(t, err)
		e.mineSeq(t, 1)
		e.force(t, "the buyer's claim after the cancel", "bad-txns-inputs-missingorspent", s.claimTx(t))
		if !e.confirmed(t, xid) {
			t.Fatal("the cancel is not confirmed")
		}
	})

	t.Run("seller_after_reorg_that_keeps_the_payment", func(t *testing.T) {
		s := e.setup(t, DefaultCoinAge, false)
		cid, err := e.send(t, s.claimTx(t))
		nlMust(t, err)
		e.mineSeq(t, 1)
		ga := e.hdr(t, s.lockBlock).AnchorHeight
		if ga <= s.fHeight {
			t.Fatalf("lock anchor %d not above F's height %d", ga, s.fHeight)
		}
		tip := e.btcTip(t)
		var bh string
		nlMust(t, e.btc.Call(&bh, "getblockhash", ga))
		nlMust(t, e.btc.Call(nil, "invalidateblock", bh))
		e.mineBtcFresh(t, int(tip-ga+2))
		var fc struct {
			Confirmations int64 `json:"confirmations"`
		}
		nlMust(t, e.btc.Call(&fc, "getrawtransaction", s.fID, true))
		t.Logf("Bitcoin reorganized from %d; F still has %d confirmations", ga, fc.Confirmations)
		e.waitDisconnected(t, s.lockBlock)
		e.force(t, "the returned lock and a cancel with F's own signature", "Script failed an OP_VERIFY operation",
			s.lockHex, s.cancelTx(t, s.fPieces(t)))
		// The seller's one other way: move the holding output alone.
		alt, _ := BuildCancelTx(s.hold, s.sellerSPK, 30_000)
		a1, _ := SignWitness(alt, s.holding, s.hold.Amount, s.sh)
		a2, _ := SignWitness(alt, s.holding, s.hold.Amount, nlKey(t))
		altHex, _ := FinalizeLockTx(alt, s.holding, a1, a2)
		if _, err := e.send(t, altHex); err == nil {
			t.Fatal("the mempool took the seller's lone respend of the holding output")
		} else {
			t.Logf("the seller's respend of the holding output (mempool): %v", err)
		}
		e.force(t, "a colluding producer mines the seller's respend of the holding output", "finished with a false/empty top stack element", altHex)
		b := e.mineSeq(t, 1)[0]
		if !e.inBlock(t, b, s.lockID) {
			t.Fatal("the lock was not mined again")
		}
		if _, err := e.send(t, s.claimTx(t)); err != nil {
			t.Fatalf("the buyer's claim after the lock came back: %v", err)
		}
		e.mineSeq(t, 1)
		if !e.confirmed(t, cid) {
			t.Fatal("the buyer's claim is not confirmed")
		}
		t.Logf("RESULT: the seller could not withdraw the lock; the buyer's claim %s confirmed again", cid)
	})

	t.Run("young_coin_is_refused_by_the_depth_check", func(t *testing.T) {
		// The residual the seller's depth check exists for: a reorganization below
		// C's own block replaces C's parent, so no Pb signature ever exists.
		s := e.setup(t, 1, false)
		if err := CheckCoinDepth(s.cHeight, s.fHeight, DefaultCoinAge); err == nil {
			t.Fatalf("a coin %d block(s) below F passed a depth check of %d", s.fHeight-s.cHeight, DefaultCoinAge)
		} else {
			t.Logf("depth check: %v", err)
		}
		cid, err := e.send(t, s.claimTx(t))
		nlMust(t, err)
		e.mineSeq(t, 1)
		tip := e.btcTip(t)
		var bh string
		nlMust(t, e.btc.Call(&bh, "getblockhash", s.cHeight))
		nlMust(t, e.btc.Call(nil, "invalidateblock", bh))
		var parent struct {
			Vin []struct {
				Txid string `json:"txid"`
				Vout uint32 `json:"vout"`
			} `json:"vin"`
		}
		nlMust(t, e.btc.Call(&parent, "getrawtransaction", s.c.Hash.String(), true))
		var ins []map[string]interface{}
		total := 0.0
		for _, v := range parent.Vin {
			ins = append(ins, map[string]interface{}{"txid": v.Txid, "vout": v.Vout})
			var pt struct {
				Vout []struct {
					Value float64 `json:"value"`
				} `json:"vout"`
			}
			nlMust(t, e.btc.Call(&pt, "getrawtransaction", v.Txid, true))
			total += pt.Vout[v.Vout].Value
		}
		var dest, raw string
		nlMust(t, e.btc.WithWallet("w").Call(&dest, "getnewaddress"))
		nlMust(t, e.btc.Call(&raw, "createrawtransaction", ins, []map[string]interface{}{{dest: fmt.Sprintf("%.8f", total-0.01)}}, 0, true))
		var signed struct {
			Hex string `json:"hex"`
		}
		nlMust(t, e.btc.WithWallet("w").Call(&signed, "signrawtransactionwithwallet", raw))
		var did string
		nlMust(t, e.btc.Call(&did, "sendrawtransaction", signed.Hex))
		t.Logf("C's block %d invalidated; C's parent double-spent by %s: no Pb signature exists", s.cHeight, did)
		e.mineBtcFresh(t, int(tip-s.cHeight+2))
		var out interface{}
		if err := e.btc.Call(&out, "gettxout", s.c.Hash.String(), s.c.Index, true); err == nil && out != nil {
			t.Logf("C still exists after the reorganization (the parent was mined again)")
		}
		e.waitDisconnected(t, s.lockBlock)
		e.mineSeq(t, 2)
		_, _ = e.send(t, s.claimTx(t))
		e.mineSeq(t, 1)
		t.Logf("RESULT (residual, refused up front by the depth check): buyer's claim confirmed=%v", e.confirmed(t, cid))
	})
}

// nlHighS returns the high-S twin of a DER signature.
func nlHighS(der []byte) []byte {
	rlen := int(der[3])
	r := der[4 : 4+rlen]
	var sv btcec.ModNScalar
	sv.SetByteSlice(der[6+rlen:])
	sv.Negate()
	b := sv.Bytes()
	hs := b[:]
	if hs[0]&0x80 != 0 {
		hs = append([]byte{0}, hs...)
	}
	out := append([]byte{0x30, byte(4 + len(r) + len(hs)), 0x02, byte(len(r))}, r...)
	out = append(out, 0x02, byte(len(hs)))
	return append(out, hs...)
}
