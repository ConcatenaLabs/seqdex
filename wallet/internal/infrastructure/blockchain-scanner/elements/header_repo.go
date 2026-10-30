package elements_scanner

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/vulpemventures/go-elements/block"
	"github.com/vulpemventures/neutrino-elements/pkg/repository"
)

// SEQUENTIA: Sequentia block headers are NOT parseable by go-elements'
// block.DeserializeHeader. On anchored chains (g_con_bitcoin_anchor, the
// default on real testnet/mainnet) the dynafed header inserts 36 extra bytes
// (m_anchor_height uint32 + m_anchor_hash uint256) between block_height and the
// dynafed params; go-elements knows nothing about them, so it misparses the
// header (typically failing with "bad serialize type for dynafed parameters")
// and, even when it does not error, recomputes a wrong block hash because its
// SerializeForHash omits those fields. See src/primitives/block.h
// (CBlockHeader::Unserialize) for the real wire format.
//
// We therefore never deserialize raw header bytes here. Instead we ask the node
// to parse its own header via `getblockheader <hash> true` (verbosity 1, JSON)
// and read the structural fields we need from the JSON. go-elements is used
// only for transactions, which are standard Elements and parse fine.

type headersRepo struct {
	rpcClient *rpcClient
	// shared by every account's scanner (see boundedCache): the tip for a second,
	// a header by its hash for good, a height's hash for a few seconds (a reorg
	// changes it)
	tip      *boundedCache[string, tipEntry]
	headers  *boundedCache[string, *block.Header]
	byHeight *boundedCache[uint32, *chainhash.Hash]
}

type tipEntry struct {
	hash   string
	header *block.Header
}

func NewHeadersRepo(
	rpcClient *rpcClient,
) repository.BlockHeaderRepository {
	return newHeadersRepo(rpcClient)
}

func newHeadersRepo(
	rpcClient *rpcClient,
) *headersRepo {
	return &headersRepo{
		rpcClient: rpcClient,
		tip:       newBoundedCache[string, tipEntry](1, time.Second),
		headers:   newBoundedCache[string, *block.Header](8192, 0),
		byHeight:  newBoundedCache[uint32, *chainhash.Hash](4096, 5*time.Second),
	}
}

func (r *headersRepo) ChainTip(
	_ context.Context,
) (*block.Header, error) {
	t, err := r.bestTip()
	if err != nil {
		return nil, err
	}
	return t.header, nil
}

// bestTip is the node's best block, read at most once a second for every
// scanner together.
func (r *headersRepo) bestTip() (tipEntry, error) {
	return r.tip.getOrLoad("", func() (tipEntry, error) {
		resp, err := r.rpcClient.call("getbestblockhash", nil)
		if err != nil {
			return tipEntry{}, err
		}
		hash, ok := resp.(string)
		if !ok {
			return tipEntry{}, fmt.Errorf("getbestblockhash gave %v", resp)
		}
		header, err := r.getHeader(hash)
		if err != nil {
			return tipEntry{}, err
		}
		if header == nil {
			return tipEntry{}, repository.ErrNoBlocksHeaders
		}
		return tipEntry{hash: hash, header: header}, nil
	}, nil)
}

func (r *headersRepo) GetBlockHeader(
	_ context.Context, hash chainhash.Hash,
) (*block.Header, error) {
	header, err := r.getHeader(hash.String())
	if err != nil {
		return nil, err
	}
	if header == nil {
		return nil, repository.ErrBlockNotFound
	}
	return header, nil
}

func (r *headersRepo) GetBlockHashByHeight(
	_ context.Context, height uint32,
) (*chainhash.Hash, error) {
	return r.getHeaderByHeight(height)
}

func (r *headersRepo) WriteHeaders(
	context.Context, ...block.Header,
) error {
	return nil
}

func (r *headersRepo) LatestBlockLocator(
	ctx context.Context,
) (blockchain.BlockLocator, error) {
	resp, err := r.rpcClient.call("getbestblockhash", nil)
	if err != nil {
		return nil, err
	}
	tipHash, err := chainhash.NewHashFromStr(resp.(string))
	if err != nil {
		return nil, err
	}
	tip, err := r.getHeader(tipHash.String())
	if err != nil {
		return nil, err
	}
	if tip == nil {
		return nil, repository.ErrNoBlocksHeaders
	}
	return r.blockLocatorFromHeader(tipHash, tip)
}

func (r *headersRepo) HasAllAncestors(
	_ context.Context, hash chainhash.Hash,
) (bool, error) {
	header, err := r.getHeader(hash.String())
	if err != nil {
		return false, err
	}
	if header == nil {
		return false, nil
	}

	for header.Height > 1 {
		prevHash, err := chainhash.NewHash(header.PrevBlockHash)
		if err != nil {
			return false, err
		}
		header, err = r.getHeader(prevHash.String())
		if err != nil {
			return false, err
		}
		if header == nil {
			return false, nil
		}
	}
	return true, nil
}

// jsonHeader mirrors the fields of `getblockheader <hash> true` that the scanner
// consumes. The node parses its own (anchored, dynafed) header format, so we
// never have to deserialize raw header bytes ourselves.
type jsonHeader struct {
	Hash              string `json:"hash"`
	Height            uint32 `json:"height"`
	Version           int32  `json:"version"`
	MerkleRoot        string `json:"merkleroot"`
	Time              uint32 `json:"time"`
	PreviousBlockHash string `json:"previousblockhash"`
}

// getHeader fetches a header via JSON-RPC (`getblockheader <hash> true`) and
// builds a *block.Header populated with the structural fields the scanner reads
// (Height, Timestamp, PrevBlockHash, MerkleRoot, Version). It deliberately does
// not attempt to fill ExtData/anchor data: the scanner never re-serializes or
// re-hashes these headers (block hashes everywhere come straight from the node
// via getblockhash/getbestblockhash), so the omitted fields are unused.
func (r *headersRepo) getHeader(hash string) (*block.Header, error) {
	// a header never changes for its hash; one not found yet is asked again
	return r.headers.getOrLoad(hash, func() (*block.Header, error) { return r.loadHeader(hash) },
		func(h *block.Header) bool { return h != nil })
}

func (r *headersRepo) loadHeader(hash string) (*block.Header, error) {
	resp, err := r.rpcClient.call("getblockheader", []interface{}{hash, true})
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "not found") {
			return nil, nil
		}
		return nil, err
	}

	jh, err := decodeJSONHeader(resp)
	if err != nil {
		return nil, err
	}

	prevBytes, err := reversedHashBytes(jh.PreviousBlockHash)
	if err != nil {
		return nil, err
	}
	merkleBytes, err := reversedHashBytes(jh.MerkleRoot)
	if err != nil {
		return nil, err
	}

	return &block.Header{
		Version:       uint32(jh.Version),
		PrevBlockHash: prevBytes,
		MerkleRoot:    merkleBytes,
		Timestamp:     jh.Time,
		Height:        jh.Height,
	}, nil
}

func (r *headersRepo) getHeaderByHeight(
	height uint32,
) (*chainhash.Hash, error) {
	return r.byHeight.getOrLoad(height, func() (*chainhash.Hash, error) { return r.loadHashByHeight(height) }, nil)
}

func (r *headersRepo) loadHashByHeight(
	height uint32,
) (*chainhash.Hash, error) {
	resp, err := r.rpcClient.call("getblockhash", []interface{}{height})
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "out of range") {
			return nil, repository.ErrBlockNotFound
		}
		return nil, err
	}

	hash := resp.(string)
	return chainhash.NewHashFromStr(hash)
}

// blockLocatorFromHeader builds a block locator rooted at the given tip. The tip
// hash is supplied explicitly (sourced from the node) rather than derived from
// header.Hash(), which go-elements computes incorrectly for anchored headers.
func (r *headersRepo) blockLocatorFromHeader(
	tipHash *chainhash.Hash, header *block.Header,
) (blockchain.BlockLocator, error) {
	var locator blockchain.BlockLocator

	// Append the initial hash
	locator = append(locator, tipHash)

	if header.Height == 0 {
		return locator, nil
	}

	height := header.Height
	decrement := uint32(1)
	for height > 0 && len(locator) < wire.MaxBlockLocatorsPerMsg {
		headerHash, err := r.getHeaderByHeight(height)
		if err != nil {
			return nil, err
		}

		locator = append(locator, headerHash)

		if decrement > height {
			height = 0
		} else {
			height -= decrement
		}

		// Decrement by 1 for the first 10 blocks, then double the jump
		// until we get to the genesis hash
		if len(locator) > 10 {
			decrement *= 2
		}
	}

	return locator, nil
}

// decodeJSONHeader coerces the decoded `getblockheader ... true` response (a
// generic map[string]interface{} produced by rpcClient.call) into jsonHeader.
func decodeJSONHeader(resp interface{}) (*jsonHeader, error) {
	m, ok := resp.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected getblockheader response type %T", resp)
	}

	jh := &jsonHeader{}
	if v, ok := m["hash"].(string); ok {
		jh.Hash = v
	}
	if v, ok := m["height"].(float64); ok {
		jh.Height = uint32(v)
	}
	if v, ok := m["version"].(float64); ok {
		jh.Version = int32(v)
	}
	if v, ok := m["merkleroot"].(string); ok {
		jh.MerkleRoot = v
	}
	if v, ok := m["time"].(float64); ok {
		jh.Time = uint32(v)
	}
	if v, ok := m["previousblockhash"].(string); ok {
		jh.PreviousBlockHash = v
	}
	return jh, nil
}

// reversedHashBytes decodes a big-endian display hash (as returned by the RPC in
// hex) into the little-endian internal byte order used by go-elements' Header
// fields (PrevBlockHash, MerkleRoot). Returns a 32-byte zero slice for an empty
// string (e.g. the genesis block has no previousblockhash).
func reversedHashBytes(h string) ([]byte, error) {
	if h == "" {
		return make([]byte, chainhash.HashSize), nil
	}
	ch, err := chainhash.NewHashFromStr(h)
	if err != nil {
		return nil, err
	}
	return ch.CloneBytes(), nil
}

// chainTipHash returns the node's current best block hash without recomputing it
// from header bytes. Used by GetLatestBlock, which must report the real hash.
func (r *headersRepo) chainTipHash() (*chainhash.Hash, uint32, error) {
	resp, err := r.rpcClient.call("getbestblockhash", nil)
	if err != nil {
		return nil, 0, err
	}
	hashStr := resp.(string)
	ch, err := chainhash.NewHashFromStr(hashStr)
	if err != nil {
		return nil, 0, err
	}
	header, err := r.getHeader(hashStr)
	if err != nil {
		return nil, 0, err
	}
	if header == nil {
		return nil, 0, repository.ErrNoBlocksHeaders
	}
	return ch, header.Height, nil
}
