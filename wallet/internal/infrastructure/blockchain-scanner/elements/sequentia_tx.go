package elements_scanner

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/vulpemventures/go-elements/transaction"
)

// SEQUENTIA: an asset issuance serializes one byte more than in Elements.
// CAssetIssuance ends with nDenomination, written after the inflation keys
// (src/primitives/confidential.h in the Sequentia repo). go-elements does not
// know that byte: it reads it as the first byte of the next field, then either
// fails ("Invalid prefix N") or, when the misread bytes happen to form a huge
// length, panics in makeslice. Every transaction carrying an issuance is
// affected, from the genesis block on.
//
// decodeSequentiaTx removes those bytes before handing the transaction to
// go-elements, so inputs, outputs, witnesses and proofs decode as usual. What
// it cannot fix is the hash: the go-elements Transaction serializes without the
// denominations, so for a transaction with an issuance TxHash() differs from
// the network txid. Callers that know the network txid record it with
// rememberNetworkTx, and readers of a decoded transaction look it up with
// networkTx.

const (
	outpointIndexNull = 0xffffffff
	outpointIssuance  = 1 << 31
)

// decodeSequentiaTx decodes a Sequentia transaction. A panic inside the
// go-elements deserializer comes back as an error.
func decodeSequentiaTx(txHex string) (tx *transaction.Transaction, err error) {
	raw, err := hex.DecodeString(txHex)
	if err != nil {
		return nil, err
	}
	stripped, err := stripIssuanceDenominations(raw)
	if err != nil {
		return nil, err
	}

	defer func() {
		if r := recover(); r != nil {
			tx, err = nil, fmt.Errorf("go-elements deserializer panicked: %v", r)
		}
	}()
	return transaction.NewTxFromBuffer(bytes.NewBuffer(stripped))
}

// stripIssuanceDenominations returns raw without the denomination byte of each
// issuance. It walks only the inputs, which is where issuances live; everything
// after them is copied unchanged.
func stripIssuanceDenominations(raw []byte) ([]byte, error) {
	c := &txCursor{buf: raw}

	// version (4) + Elements witness flag (1)
	if err := c.skip(5); err != nil {
		return nil, err
	}
	nIn, err := c.varInt()
	if err != nil {
		return nil, err
	}
	if nIn > uint64(len(raw)) {
		return nil, fmt.Errorf("input count %d exceeds tx size", nIn)
	}

	drop := make([]int, 0)
	for i := uint64(0); i < nIn; i++ {
		if err := c.skip(32); err != nil { // prevout hash
			return nil, err
		}
		index, err := c.uint32()
		if err != nil {
			return nil, err
		}
		if err := c.skipVarSlice(); err != nil { // scriptSig
			return nil, err
		}
		if err := c.skip(4); err != nil { // sequence
			return nil, err
		}
		if index == outpointIndexNull || index&outpointIssuance == 0 {
			continue
		}
		// blinding nonce + entropy, then amount and inflation keys
		if err := c.skip(64); err != nil {
			return nil, err
		}
		if err := c.skipConfidentialValue(); err != nil {
			return nil, err
		}
		if err := c.skipConfidentialValue(); err != nil {
			return nil, err
		}
		drop = append(drop, c.pos)
		if err := c.skip(1); err != nil { // nDenomination
			return nil, err
		}
	}

	if len(drop) == 0 {
		return raw, nil
	}
	out := make([]byte, 0, len(raw)-len(drop))
	prev := 0
	for _, p := range drop {
		out = append(out, raw[prev:p]...)
		prev = p + 1
	}
	return append(out, raw[prev:]...), nil
}

type txCursor struct {
	buf []byte
	pos int
}

func (c *txCursor) skip(n uint64) error {
	if n > uint64(len(c.buf)-c.pos) {
		return fmt.Errorf("tx truncated at byte %d", c.pos)
	}
	c.pos += int(n)
	return nil
}

func (c *txCursor) uint32() (uint32, error) {
	if len(c.buf)-c.pos < 4 {
		return 0, fmt.Errorf("tx truncated at byte %d", c.pos)
	}
	v := binary.LittleEndian.Uint32(c.buf[c.pos:])
	c.pos += 4
	return v, nil
}

func (c *txCursor) varInt() (uint64, error) {
	if c.pos >= len(c.buf) {
		return 0, fmt.Errorf("tx truncated at byte %d", c.pos)
	}
	prefix := c.buf[c.pos]
	c.pos++
	size := 0
	switch prefix {
	case 0xfd:
		size = 2
	case 0xfe:
		size = 4
	case 0xff:
		size = 8
	default:
		return uint64(prefix), nil
	}
	if len(c.buf)-c.pos < size {
		return 0, fmt.Errorf("tx truncated at byte %d", c.pos)
	}
	var v uint64
	for i := size - 1; i >= 0; i-- {
		v = v<<8 | uint64(c.buf[c.pos+i])
	}
	c.pos += size
	return v, nil
}

func (c *txCursor) skipVarSlice() error {
	n, err := c.varInt()
	if err != nil {
		return err
	}
	return c.skip(n)
}

// skipConfidentialValue skips a null (1 byte), explicit (9) or committed (33)
// value.
func (c *txCursor) skipConfidentialValue() error {
	if c.pos >= len(c.buf) {
		return fmt.Errorf("tx truncated at byte %d", c.pos)
	}
	switch prefix := c.buf[c.pos]; prefix {
	case 0:
		return c.skip(1)
	case 1:
		return c.skip(9)
	case 8, 9:
		return c.skip(33)
	default:
		return fmt.Errorf("invalid confidential value prefix %d at byte %d", prefix, c.pos)
	}
}

// networkTxs maps the hash go-elements computes for a decoded transaction to
// the network txid and the original hex, for the transactions where the two
// differ (those carrying an issuance). It only grows with issuance
// transactions found in blocks that match the wallet, so it stays small.
var networkTxs sync.Map

type sequentiaTx struct {
	txid  string
	txHex string
}

func rememberNetworkTx(tx *transaction.Transaction, txid, txHex string) {
	if h := tx.TxHash().String(); h != txid {
		networkTxs.Store(h, sequentiaTx{txid: txid, txHex: txHex})
	}
}

// networkTx returns the network txid and hex of a decoded transaction.
func networkTx(tx *transaction.Transaction) (txid, txHex string) {
	txid = tx.TxHash().String()
	if v, ok := networkTxs.Load(txid); ok {
		n := v.(sequentiaTx)
		return n.txid, n.txHex
	}
	txHex, _ = tx.ToHex()
	return txid, txHex
}
