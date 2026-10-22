package watcher

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
)

// RoundSource tells a fill made by an Arca operator's round from any other.
// A round pays each maker on the chain, as any fill does, but its taker's side
// is a leaf in the round's batch, so no output of the round pays the taker:
// the record of such a fill names the operator and the round instead.
type RoundSource interface {
	// Round reports whether txid, a transaction with these outputs, is a round
	// of an operator the relay follows, with the operator's key and the round's
	// batch outputs. ok is false for any other transaction.
	Round(txid string, outputs []covenant.Output) (r RoundInfo, ok bool, err error)
}

// RoundInfo is an operator's round, as the operator publishes it and as the
// round's own outputs confirm it.
type RoundInfo struct {
	Operator   string // the operator's key, x-only hex (its /v1/info "operator")
	URL        string
	BatchVouts []uint32
}

// ArcaRounds follows Arca operators over their public API: GET /v1/info for
// the operator's key and chain, POST /v1/rounds for the rounds it has built,
// POST /v1/tree for a batch's published tree. A transaction is taken as an
// operator's round only when that operator lists it, the operator runs on the
// relay's chain (its genesis hash), and each batch output the operator names
// pays the root of the tree it publishes for it.
type ArcaRounds struct {
	genesis string
	http    *http.Client
	ops     []*arcaOperator
}

type arcaOperator struct {
	url string

	mu       sync.Mutex
	key      string
	genesis  string
	cursor   string
	rounds   map[string][]uint32 // round txid -> batch outputs
	order    []string            // round txids, oldest first, as the operator lists them
	verified map[string]bool     // round txid -> its batch outputs pay its published trees
	lastPage time.Time

	// For orders resting in its tree (resting.go).
	marginSecs   int64              // fills.order_margin_seconds
	deadlineSecs int64              // participations.exit_deadline_seconds
	leafAt       map[string]leafPos // leaf id -> the batch output holding it
	scanned      map[string]bool    // "txid:vout" of trees read for leaf ids
}

// NewArcaRounds follows the operators at urls (each the base URL of an Arca
// server, e.g. http://127.0.0.1:3535), on the chain whose genesis block hash
// is genesis (display hex).
func NewArcaRounds(urls []string, genesis string) *ArcaRounds {
	a := &ArcaRounds{genesis: strings.ToLower(genesis), http: &http.Client{Timeout: 10 * time.Second}}
	for _, u := range urls {
		if u = strings.TrimRight(strings.TrimSpace(u), "/"); u != "" {
			a.ops = append(a.ops, &arcaOperator{url: u, rounds: map[string][]uint32{}, verified: map[string]bool{},
				leafAt: map[string]leafPos{}, scanned: map[string]bool{}})
		}
	}
	return a
}

func (a *ArcaRounds) call(method, url string, body interface{}, out interface{}) error {
	var rd *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("%s: http %d %s %s", url, resp.StatusCode, e.Code, e.Message)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Round implements RoundSource.
func (a *ArcaRounds) Round(txid string, outputs []covenant.Output) (RoundInfo, bool, error) {
	var lastErr error
	for _, op := range a.ops {
		r, ok, err := a.roundOf(op, strings.ToLower(txid), outputs)
		if err != nil {
			lastErr = err
			continue
		}
		if ok {
			return r, true, nil
		}
	}
	return RoundInfo{}, false, lastErr
}

// minRepage keeps a relay from paging an operator's rounds more than once a
// second while it classifies a burst of fills.
const minRepage = time.Second

func (a *ArcaRounds) roundOf(op *arcaOperator, txid string, outputs []covenant.Output) (RoundInfo, bool, error) {
	op.mu.Lock()
	defer op.mu.Unlock()
	if err := a.infoOf(op); err != nil {
		return RoundInfo{}, false, err
	}
	if a.genesis != "" && op.genesis != a.genesis {
		return RoundInfo{}, false, nil // an operator on another chain fills nothing here
	}
	batches, listed := op.rounds[txid]
	if !listed && time.Since(op.lastPage) >= minRepage {
		if err := a.pageRounds(op); err != nil {
			return RoundInfo{}, false, err
		}
		batches, listed = op.rounds[txid]
	}
	if !listed {
		return RoundInfo{}, false, nil
	}
	if _, seen := op.verified[txid]; !seen {
		ok, err := a.batchesPayTheirTrees(op, txid, batches, outputs)
		if err != nil {
			return RoundInfo{}, false, err
		}
		op.verified[txid] = ok
	}
	if !op.verified[txid] {
		return RoundInfo{}, false, nil
	}
	return RoundInfo{Operator: op.key, URL: op.url, BatchVouts: append([]uint32(nil), batches...)}, true, nil
}

// infoOf reads the operator's /v1/info once: its key, its chain, and the
// times its resting orders keep to. The caller holds op.mu.
func (a *ArcaRounds) infoOf(op *arcaOperator) error {
	if op.key != "" {
		return nil
	}
	var info struct {
		Operator    string `json:"operator"`
		GenesisHash string `json:"genesis_hash"`
		Fills       struct {
			OrderMarginSeconds int64 `json:"order_margin_seconds"`
		} `json:"fills"`
		Participations struct {
			ExitDeadlineSeconds int64 `json:"exit_deadline_seconds"`
		} `json:"participations"`
	}
	if err := a.call(http.MethodGet, op.url+"/v1/info", nil, &info); err != nil {
		return err
	}
	op.key, op.genesis = strings.ToLower(info.Operator), strings.ToLower(info.GenesisHash)
	op.marginSecs, op.deadlineSecs = info.Fills.OrderMarginSeconds, info.Participations.ExitDeadlineSeconds
	return nil
}

// pageRounds reads the rounds the operator built since the last page read.
// The caller holds op.mu.
func (a *ArcaRounds) pageRounds(op *arcaOperator) error {
	op.lastPage = time.Now()
	for page := 0; page < 50; page++ {
		req := map[string]interface{}{}
		if op.cursor != "" {
			req["after"] = op.cursor
		}
		var res struct {
			Rounds []struct {
				Cursor  string   `json:"cursor"`
				Txid    string   `json:"txid"`
				Batches []uint32 `json:"batches"`
			} `json:"rounds"`
			Next string `json:"next"`
		}
		if err := a.call(http.MethodPost, op.url+"/v1/rounds", req, &res); err != nil {
			return err
		}
		for _, r := range res.Rounds {
			t := strings.ToLower(r.Txid)
			if _, seen := op.rounds[t]; !seen {
				op.order = append(op.order, t)
			}
			op.rounds[t] = r.Batches
		}
		if len(res.Rounds) == 0 || res.Next == "" {
			break
		}
		op.cursor = res.Next
	}
	return nil
}

// batchesPayTheirTrees checks each batch output the operator names against the
// tree it publishes for it: the output exists, and pays the script of the
// tree's root (the last level of its nodes) in the tree's asset.
func (a *ArcaRounds) batchesPayTheirTrees(op *arcaOperator, txid string, batches []uint32, outputs []covenant.Output) (bool, error) {
	if len(batches) == 0 {
		return false, nil
	}
	for _, v := range batches {
		if int(v) >= len(outputs) {
			return false, nil
		}
		var t struct {
			Asset string `json:"asset"`
			Nodes [][]struct {
				ScriptPubkey string `json:"script_pubkey"`
			} `json:"nodes"`
		}
		if err := a.call(http.MethodPost, op.url+"/v1/tree", map[string]interface{}{"txid": txid, "vout": v}, &t); err != nil {
			return false, err
		}
		if len(t.Nodes) == 0 || len(t.Nodes[len(t.Nodes)-1]) != 1 {
			return false, nil
		}
		root, err := hex.DecodeString(t.Nodes[len(t.Nodes)-1][0].ScriptPubkey)
		if err != nil || !bytes.Equal(root, outputs[v].Script) {
			return false, nil
		}
		if o := outputs[v]; !o.AssetExplicit || !strings.EqualFold(displayOf(o.Asset), t.Asset) {
			return false, nil
		}
	}
	return true, nil
}

func hexDecode(s string) ([]byte, error) { return hex.DecodeString(s) }
