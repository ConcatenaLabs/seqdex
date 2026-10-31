package elements_scanner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aejkcs50/seqdex/wallet/internal/core/domain"
	"github.com/aejkcs50/seqdex/wallet/internal/core/ports"
	"github.com/aejkcs50/seqdex/wallet/pkg/seqnet"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/vulpemventures/go-elements/address"
	"github.com/vulpemventures/go-elements/elementsutil"
	"github.com/vulpemventures/go-elements/network"
	"github.com/vulpemventures/go-elements/transaction"
	"github.com/vulpemventures/neutrino-elements/pkg/blockservice"
	"github.com/vulpemventures/neutrino-elements/pkg/repository"
)

type service struct {
	args        ServiceArgs
	rpcClient   *rpcClient
	blockSvc    blockservice.BlockService
	scanners    map[string]*scannerService
	genesisHash *chainhash.Hash

	filtersRepo repository.FilterRepository
	headersRepo repository.BlockHeaderRepository
	lock        *sync.RWMutex

	quit      chan struct{}
	// checkpoints: each account's scanned height as the last run saved it (CheckpointFile)
	checkpoints map[string]uint32
}

type ServiceArgs struct {
	RpcAddr string
	Network string
	// CheckpointFile is optional: where each account's scanned height is kept, so a restart
	// resumes there instead of scanning again from the account's birth.
	CheckpointFile string
	// EsploraUrl is optional. When empty the scanner fetches blocks directly
	// from the node via JSON-RPC (node-RPC-only mode); when set it uses an
	// external Esplora HTTP endpoint instead. The filters and headers repos are
	// always node-RPC-backed, so no on-disk datadirs are needed here.
	EsploraUrl string
}

func (a ServiceArgs) validate() error {
	if a.RpcAddr == "" {
		return fmt.Errorf("missing elements node rpc address to connect to")
	}
	if a.Network == "" {
		return fmt.Errorf("missing network")
	}
	// EsploraUrl is optional: when empty the scanner uses the node-RPC block
	// service instead of Esplora. No datadir requirements: the filters/headers
	// repos are node-RPC-backed.
	return nil
}

func (a ServiceArgs) network() network.Network {
	if n, ok := seqnet.ByName(a.Network); ok {
		return n
	}
	return seqnet.SequentiaTestnet
}

func NewElementsScanner(args ServiceArgs) (ports.BlockchainScanner, error) {
	if err := args.validate(); err != nil {
		return nil, err
	}

	// SEQUENTIA: the previous 5s HTTP timeout was too low — large blocks (and
	// the initial getblockchaininfo handshake while the node is busy) could
	// exceed it, making CreateWallet/GetLatestBlock fail at ~5s. Use a generous
	// fixed default instead.
	rpcClient, err := newRpcClient(args.RpcAddr, rpcTimeoutSeconds)
	if err != nil {
		return nil, err
	}
	filtersDb := newFiltersRepo(rpcClient)
	headersDb := newHeadersRepo(rpcClient)

	// Node-RPC-only by default: fetch blocks via JSON-RPC. Fall back to Esplora
	// only when an EsploraUrl is explicitly configured.
	var blockSvc blockservice.BlockService
	if args.EsploraUrl != "" {
		blockSvc = blockservice.NewEsploraBlockService(args.EsploraUrl)
	} else {
		blockSvc = NewRpcBlockService(rpcClient)
	}
	// Fetch the real genesis hash from the node rather than relying on the
	// hardcoded neutrino-elements checkpoints (which only know Liquid networks
	// and would otherwise yield a wrong genesis for Sequentia).
	genesisHash, err := fetchGenesisHash(headersDb)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch genesis block hash: %s", err)
	}

	scanners := make(map[string]*scannerService)
	lock := &sync.RWMutex{}
	return &service{
		args:        args,
		rpcClient:   rpcClient,
		blockSvc:    blockSvc,
		scanners:    scanners,
		genesisHash: genesisHash,
		filtersRepo: filtersDb,
		checkpoints: loadCheckpoints(args.CheckpointFile),
		headersRepo: headersDb,
		lock:        lock,
		quit:        make(chan struct{}),
	}, nil
}

// Start starts nothing of its own: each account's scanner follows the node's
// tip by itself, its watches waiting at the first block not yet scanned and
// scanned as soon as the node has it.
// Start saves each account's scanned height every minute (CheckpointFile), and once more at
// Stop. Each account's scanner follows the node's tip by itself.
func (s *service) Start() {
	if s.args.CheckpointFile == "" {
		return
	}
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-s.quit:
				return
			case <-t.C:
				s.saveCheckpoints()
			}
		}
	}()
}

func (s *service) Stop() {
	s.saveCheckpoints()
	close(s.quit)
}

// checkpointMargin is how far below its saved height an account resumes: a reorg of the
// last blocks before a stop is scanned again.
const checkpointMargin = 12

func loadCheckpoints(path string) map[string]uint32 {
	out := map[string]uint32{}
	if path == "" {
		return out
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	_ = json.Unmarshal(b, &out)
	return out
}

// saveCheckpoints writes every account's lowest unscanned height (written whole, then renamed).
func (s *service) saveCheckpoints() {
	if s.args.CheckpointFile == "" {
		return
	}
	s.lock.RLock()
	out := map[string]uint32{}
	for name, sc := range s.scanners {
		if p, ok := sc.svc.(interface{ LowestUnscanned() (uint32, bool) }); ok {
			if h, ok := p.LowestUnscanned(); ok {
				out[name] = h
			}
		}
	}
	s.lock.RUnlock()
	b, _ := json.Marshal(out)
	tmp := s.args.CheckpointFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		_ = os.Rename(tmp, s.args.CheckpointFile)
	}
}

// resumeHeight is where an account's watches start after a restart: its saved height less the
// margin, never below its birth.
func resumeHeight(saved map[string]uint32, account string, birth uint32) uint32 {
	h, ok := saved[account]
	if !ok || h <= checkpointMargin {
		return birth
	}
	if h-checkpointMargin > birth {
		return h - checkpointMargin
	}
	return birth
}

func (s *service) GetUtxoChannel(accountName string) chan []*domain.Utxo {
	scannerSvc := s.getOrCreateScanner(accountName, 0)
	return scannerSvc.chUtxos
}

func (s *service) GetTxChannel(accountName string) chan *domain.Transaction {
	scannerSvc := s.getOrCreateScanner(accountName, 0)
	return scannerSvc.chTxs
}

func (s *service) WatchForAccount(
	accountName string, startingBlock uint32, addressesInfo []domain.AddressInfo,
) {
	scannerSvc := s.getOrCreateScanner(accountName, startingBlock)
	scannerSvc.watchAddresses(addressesInfo)
}

func (s *service) WatchForUtxos(
	accountName string, utxos []domain.UtxoInfo,
) {
	scannerSvc := s.getOrCreateScanner(accountName, 0)
	scannerSvc.watchUtxos(utxos)
}

func (s *service) RestoreAccount(
	accountIndex uint32, accountName, xpub string, masterBlindingKey []byte,
	startingBlockHeight, _ uint32,
) ([]domain.AddressInfo, []domain.AddressInfo, error) {
	return nil, nil, fmt.Errorf("not implemented")
}

func (s *service) StopWatchForAccount(accountName string) {
	scannerSvc := s.getOrCreateScanner(accountName, 0)
	scannerSvc.stop()
	s.removeScanner(accountName)
}

func (s *service) GetUtxos(utxoList []domain.Utxo) ([]domain.Utxo, error) {
	utxos := make([]domain.Utxo, 0, len(utxoList))
	for _, u := range utxoList {
		key := u.UtxoKey
		// Fetch the prevout tx via the node-level getrawtransaction (the node runs
		// txindex), NOT importaddress+gettransaction. Those are wallet-level RPCs that
		// (a) fail with -19 "Wallet file not specified" once the node has more than one
		// wallet loaded, and (b) cannot resolve a tx the node wallet never tracked
		// (the scanner uses block filters, not node-wallet imports, so the address was
		// never imported with history). getrawtransaction needs no wallet context and
		// resolves any txid (mempool or txindexed).
		resp, err := s.rpcClient.call("getrawtransaction", []interface{}{key.TxID, true})
		if err != nil {
			return nil, fmt.Errorf("getrawtransaction %s: %w", key.TxID, err)
		}
		m, ok := resp.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("getrawtransaction %s: unexpected response", key.TxID)
		}

		txHex, _ := m["hex"].(string)
		tx, err := decodeSequentiaTx(txHex)
		if err != nil {
			return nil, fmt.Errorf("parse prevout tx %s: %w", key.TxID, err)
		}

		out := tx.Outputs[key.VOut]
		utxo := domain.Utxo{
			UtxoKey: key,
			Script:  out.Script,
		}
		if out.IsConfidential() {
			utxo.AssetCommitment = out.Asset
			utxo.ValueCommitment = out.Value
			utxo.Nonce = out.Nonce
			utxo.RangeProof = out.RangeProof
			utxo.SurjectionProof = out.SurjectionProof
		} else {
			utxo.Asset = elementsutil.AssetHashFromBytes(out.Asset)
			utxo.Value, _ = elementsutil.ValueFromBytes(out.Value)
		}
		// getrawtransaction reports confirmations + blockhash/blocktime but not the
		// height; derive it from the (node-level) block header when confirmed.
		if conf, _ := m["confirmations"].(float64); conf > 0 {
			blockHash, _ := m["blockhash"].(string)
			blockTimestamp, _ := m["blocktime"].(float64)
			var blockHeight uint64
			if blockHash != "" {
				if hdr, herr := s.rpcClient.call("getblockheader", []interface{}{blockHash}); herr == nil {
					if hm, ok := hdr.(map[string]interface{}); ok {
						if h, ok := hm["height"].(float64); ok {
							blockHeight = uint64(h)
						}
					}
				}
			}
			utxo.ConfirmedStatus = domain.UtxoStatus{
				BlockHeight: blockHeight,
				BlockTime:   int64(blockTimestamp),
				BlockHash:   blockHash,
			}
		}
		utxos = append(utxos, utxo)
	}

	return utxos, nil
}

func (s *service) GetUtxosForAddresses(
	_ []domain.AddressInfo,
) ([]*domain.Utxo, error) {
	return nil, fmt.Errorf("not implemented")
}

func (s *service) BroadcastTransaction(txHex string) (string, error) {
	if _, err := transaction.NewTxFromHex(txHex); err != nil {
		return "", fmt.Errorf("invalid tx: %s", err)
	}
	resp, err := s.rpcClient.call("sendrawtransaction", []interface{}{txHex})
	if err != nil {
		return "", err
	}
	txid := resp.(string)
	return txid, nil
}

func (s *service) GetTransactions(txids []string) ([]domain.Transaction, error) {
	return nil, fmt.Errorf("not implemented")
}

// FeeExchangeRate resolves the open-fee-market exchange rate for assetHex from
// the node (getfeeexchangerates: atoms-of-asset per exchange_rate_scale native
// atoms). A node with an asset registry keys the rate map by human LABEL (e.g.
// "GOLD") rather than the asset hex, so we also pull dumpassetlabels
// (label -> hex) and resolve label-keyed entries back to their hex before
// matching. Any RPC error, a missing asset, or a non-positive rate yields
// (0, false) so callers fall back to the native fee asset.
func (s *service) FeeExchangeRate(assetHex string) (uint64, bool) {
	resp, err := s.rpcClient.call("getfeeexchangerates", nil)
	if err != nil {
		return 0, false
	}
	rates, ok := resp.(map[string]interface{})
	if !ok {
		return 0, false
	}
	want := strings.ToLower(assetHex)

	// Best-effort label -> hex map; nil/empty if the node has no asset registry
	// (the rate keys are then already hex) or doesn't support the call.
	labels := make(map[string]string)
	if lr, lerr := s.rpcClient.call("dumpassetlabels", nil); lerr == nil {
		if lm, ok := lr.(map[string]interface{}); ok {
			for label, hexVal := range lm {
				if h, ok := hexVal.(string); ok {
					labels[label] = h
				}
			}
		}
	}

	for key, raw := range rates {
		rate, ok := raw.(float64)
		if !ok || rate <= 0 {
			continue
		}
		if strings.ToLower(key) == want {
			return uint64(rate), true
		}
		if h, ok := labels[key]; ok && strings.ToLower(h) == want {
			return uint64(rate), true
		}
	}
	return 0, false
}

func (s *service) GetLatestBlock() ([]byte, uint32, error) {
	// SEQUENTIA: take the tip hash straight from the node rather than computing
	// it via go-elements' block.Header.Hash(), which mis-serializes anchored
	// Sequentia headers (it omits m_anchor_height/m_anchor_hash) and so would
	// return a hash that does not exist on-chain.
	concrete, ok := s.headersRepo.(*headersRepo)
	if !ok {
		// Fallback for any non-node-RPC headers repo: derive from ChainTip.
		// (The default Sequentia path always uses *headersRepo.)
		block, err := s.headersRepo.ChainTip(context.Background())
		if err != nil {
			return nil, 0, err
		}
		hash, _ := block.Hash()
		return hash.CloneBytes(), block.Height, nil
	}
	hash, height, err := concrete.chainTipHash()
	if err != nil {
		return nil, 0, err
	}
	return hash.CloneBytes(), height, nil
}

func (s *service) GetBlockHash(height uint32) ([]byte, error) {
	hash, err := s.headersRepo.GetBlockHashByHeight(context.Background(), height)
	if err != nil {
		return nil, err
	}
	return hash.CloneBytes(), nil
}

func (s *service) getOrCreateScanner(
	accountName string, startingBlock uint32,
) *scannerService {
	s.lock.Lock()
	defer s.lock.Unlock()

	if scannerSvc, ok := s.scanners[accountName]; ok {
		// made before the account's birthday was known (by a caller that knows none): it
		// takes the birthday now, so its watches start there and not at genesis
		if startingBlock > 0 {
			scannerSvc.adoptBirthday(startingBlock, resumeHeight(s.checkpoints, accountName, startingBlock))
		}
		return scannerSvc
	}

	scannerSvc := newScannerSvc(
		accountName, startingBlock, s.filtersRepo, s.headersRepo, s.blockSvc,
		s.genesisHash,
	)
	// Every watch resumes where the last run had scanned the account to.
	scannerSvc.resumeFrom = resumeHeight(s.checkpoints, accountName, startingBlock)
	s.scanners[accountName] = scannerSvc
	return scannerSvc
}

func (s *service) removeScanner(accountName string) {
	s.lock.Lock()
	defer s.lock.Unlock()

	delete(s.scanners, accountName)
}

// fetchGenesisHash returns the height-0 block hash as reported by the node.
func fetchGenesisHash(
	headersRepo repository.BlockHeaderRepository,
) (*chainhash.Hash, error) {
	return headersRepo.GetBlockHashByHeight(context.Background(), 0)
}

func addressFromScript(script []byte, net network.Network) string {
	switch scriptType := address.GetScriptType(script); scriptType {
	case address.P2PkhScript, address.P2ShScript:
		prefix := net.PubKeyHash
		scriptHash := script[3 : len(script)-2]
		if scriptType == address.P2ShScript {
			prefix = net.ScriptHash
			scriptHash = script[2 : len(script)-1]
		}
		return address.ToBase58(&address.Base58{
			Version: prefix,
			Data:    scriptHash,
		})
	default:
		addr, _ := address.ToBech32(&address.Bech32{
			Prefix:  net.Bech32,
			Version: script[0],
			Program: script[2:],
		})
		return addr
	}
}
