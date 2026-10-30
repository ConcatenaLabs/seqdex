package main

// exec_pureln.go — the EMBEDDED pure-LN taker leg. Mirrors cmd/seqob-cli xpln
// (the proven wiring) but calls internal/seqob/client.RunTakerPureLN in-process:
// pure-LN has no on-chain leg and no refund state file — a failed attempt
// unwinds natively by the LN hold timeout — so embedding adds no recovery
// obligations. The relay only ever couriers ciphertext (E2E to the maker's
// signed offer key).

import (
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/encoding/protojson"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/client"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

var jsonMarshal = protojson.MarshalOptions{UseProtoNames: true}

// runPureLNLeg lifts `take` base atoms of a pure-LN offer. Ask = we BUY the
// asset (pay BTC-LN); bid = we SELL (receive BTC-LN).
func (e *Executor) runPureLNLeg(n *NormOrder, take uint64) LegResult {
	res := e.runPureLN(n, take)
	if res.Err != "" {
		res.Resume = "pure-LN leg leaves no on-chain residue: the hold unwinds by its own timeout"
	}
	return res
}

func (e *Executor) runPureLN(n *NormOrder, take uint64) LegResult {
	c := &e.Caps
	o := n.Offer
	wantDir := offer.LnBTCLNForAssetLN // ask: the maker gives the asset; we buy
	dir := client.PlnBuy
	if n.Side == SideBid {
		wantDir, dir = offer.LnAssetLNForBTCLN, client.PlnSell
	}
	if got := o.GetLightning().GetLnDirection(); got != wantDir {
		return LegResult{Err: fmt.Sprintf("pure-LN offer direction %d does not match side %s", got, n.Side)}
	}

	// asset<->asset markets: the counter leg settles over the QUOTE asset's LN
	// channels (the maker's -quote-asset idiom); a BTC-sentinel quote is real
	// BTC-LN over the btc-ln socket.
	effBtcAsset := ""
	if !offer.IsBTCSentinel(n.Quote) {
		effBtcAsset = n.Quote
	}

	assetAtoms, btcSats := plnWhole(n)
	seqMsat, btcMsat := assetAtoms*1000, btcSats*1000
	takeMsat := uint64(0)
	if take < assetAtoms {
		takeMsat = take * 1000
	}

	// Validate both LN nodes up front (fail before opening a session).
	if _, err := xchain.NewCLNAssetLNLeg(c.AssetLNSocket, n.Base).NodeID(); err != nil {
		return LegResult{Err: fmt.Sprintf("asset lightning-rpc %s unreachable: %v", c.AssetLNSocket, err)}
	}
	if _, err := xchain.NewCLNLNLeg(c.BtcLNSocket).NodeID(); err != nil {
		return LegResult{Err: fmt.Sprintf("btc lightning-rpc %s unreachable: %v", c.BtcLNSocket, err)}
	}

	// Open the swap session; E2E key from the SIGNED offer (fresh session key).
	takerKey, err := btcec.NewPrivateKey()
	if err != nil {
		return LegResult{Err: err.Error()}
	}
	wsURL := "ws" + strings.TrimPrefix(n.Relay, "http") + "/v1/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return LegResult{Err: fmt.Sprintf("dial ws %s: %v", wsURL, err)}
	}
	defer conn.Close()

	if err := writeWS(conn, &seqobv1.To{Msg: &seqobv1.To_StartLift{StartLift: &seqobv1.StartLift{
		OfferId:            o.GetOfferId(),
		MakerPubkey:        o.GetMakerPubkey(),
		TakeAmount:         o.GetBaseAmount(), // slice negotiated in-driver via TakeSeqMsat
		TakerSessionPubkey: takerKey.PubKey().SerializeCompressed(),
	}}}); err != nil {
		return LegResult{Err: fmt.Sprintf("start lift: %v", err)}
	}
	la, err := readWS(conn, 10*time.Second)
	if err != nil {
		return LegResult{Err: fmt.Sprintf("lift handshake: %v", err)}
	}
	if la.GetLiftAccepted() == nil {
		return LegResult{Err: fmt.Sprintf("expected lift_accepted, got %s", la.String())}
	}
	sid := la.GetLiftAccepted().GetSessionId()
	e.Logf("  [pureln] session %s opened on %s (%s %d of %d atoms)", sid, n.Relay, n.Side, take, assetAtoms)

	makerPubBytes, err := hex.DecodeString(o.GetMakerPubkey())
	if err != nil {
		return LegResult{Err: "decode maker pubkey: " + err.Error()}
	}
	makerPub, err := btcec.ParsePubKey(makerPubBytes)
	if err != nil {
		return LegResult{Err: "parse maker pubkey: " + err.Error()}
	}
	crypter, err := client.NewCrypter(takerKey, makerPub)
	if err != nil {
		return LegResult{Err: "crypter: " + err.Error()}
	}

	send := func(sealed []byte) error {
		b, err := jsonMarshal.Marshal(&seqobv1.To{Msg: &seqobv1.To_SwapMsg{SwapMsg: &seqobv1.SwapMsg{SessionId: sid, Ciphertext: sealed}}})
		if err != nil {
			return err
		}
		return conn.WriteMessage(websocket.TextMessage, b)
	}
	recv := startCourierReader(conn)

	var btcLeg xchain.LNLeg = xchain.NewCLNLNLeg(c.BtcLNSocket)
	if effBtcAsset != "" {
		btcLeg = xchain.NewCLNAssetLNLeg(counterLegSocket(c, effBtcAsset), effBtcAsset)
	}
	swap := xchain.NewPureLNSwap(xchain.NewCLNAssetLNLeg(c.AssetLNSocket, n.Base), btcLeg)
	var ops client.PlnTakerOps
	if dir == client.PlnBuy {
		ops = &client.LivePlnTakerBuyOps{Swap: swap}
	} else {
		ops = &client.LivePlnTakerSellOps{Swap: swap}
	}

	r, err := client.RunTakerPureLN(client.TakerPlnParams{
		Direction:   dir,
		Ops:         ops,
		Crypter:     crypter,
		BtcAmtMsat:  btcMsat,
		SeqAmtMsat:  seqMsat,
		TakeSeqMsat: takeMsat,
		FinalCltv:   18,
		Timing:      client.XcTiming{TermsWait: 2 * time.Minute, SeqLockWait: 2 * time.Minute},
		Log:         func(f string, a ...interface{}) { e.Logf("  [pureln] "+f, a...) },
	}, send, recv)
	if err != nil {
		return LegResult{Err: "pure-LN swap: " + err.Error()}
	}
	e.Logf("  [pureln] SETTLED %d atoms for %d sats (preimage %s)",
		r.FilledSeqMsat/1000, r.FilledBtcMsat/1000, hex.EncodeToString(r.Preimage))
	return LegResult{Settled: true}
}

// --- minimal WS helpers (the seqob-cli idiom, package-local) ----------------

func writeWS(c *websocket.Conn, to *seqobv1.To) error {
	b, err := jsonMarshal.Marshal(to)
	if err != nil {
		return err
	}
	return c.WriteMessage(websocket.TextMessage, b)
}

func readWS(c *websocket.Conn, timeout time.Duration) (*seqobv1.From, error) {
	c.SetReadDeadline(time.Now().Add(timeout))
	_, data, err := c.ReadMessage()
	if err != nil {
		return nil, err
	}
	var from seqobv1.From
	if err := jsonUnmarshal.Unmarshal(data, &from); err != nil {
		return nil, err
	}
	return &from, nil
}

// startCourierReader is the single background WS reader (one reader keeps
// gorilla auto-ponging the relay's keepalive through any driver wait — the same
// reasoning as seqob-cli's startCourierReader).
func startCourierReader(c *websocket.Conn) client.XcRecv {
	frames := make(chan *seqobv1.From, 16)
	errc := make(chan error, 1)
	go func() {
		c.SetReadDeadline(time.Time{})
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				errc <- err
				close(frames)
				return
			}
			var from seqobv1.From
			if jsonUnmarshal.Unmarshal(data, &from) != nil {
				continue
			}
			frames <- &from
		}
	}()
	return func(timeout time.Duration) ([]byte, error) {
		deadline := time.After(timeout)
		for {
			select {
			case f, ok := <-frames:
				if !ok {
					select {
					case e := <-errc:
						return nil, e
					default:
						return nil, fmt.Errorf("courier connection closed")
					}
				}
				if f.GetSwapMsg() != nil {
					return f.GetSwapMsg().GetCiphertext(), nil
				}
				if e := f.GetError(); e != nil {
					return nil, fmt.Errorf("relay error %d: %s", e.GetCode(), e.GetMessage())
				}
			case <-deadline:
				return nil, fmt.Errorf("courier recv timeout")
			case e := <-errc:
				return nil, e
			}
		}
	}
}

// counterLegSocket is the node the counter leg of a pure-LN take settles on: real BTC on the
// bitcoin node, an asset quote (an asset<->asset market) on the asset node, where every asset's
// channels are. The bitcoin node knows no asset: an asset invoice asked of it is refused ("assets
// are only supported" on Elements chains) and the maker's asset node is not in its graph, so every
// asset<->asset take failed there.
func counterLegSocket(c *Caps, quoteAsset string) string {
	if quoteAsset == "" || offer.IsBTCSentinel(quoteAsset) {
		return c.BtcLNSocket
	}
	return c.AssetLNSocket
}

// plnWhole is the offer's signed whole, asset atoms and sats, that both sides of a pure-LN take
// price a slice from. BaseSize is capped to what the relay says is left of a partly filled offer
// while QuoteNum stays the signed whole, so a ratio of the two priced a slice of a partly filled
// offer off a whole the maker never signed, and the maker's terms were rejected ("maker BTC
// amount N != the N the offer's ratio gives").
func plnWhole(n *NormOrder) (assetAtoms, btcSats uint64) {
	return n.BaseDen, n.QuoteNum
}
