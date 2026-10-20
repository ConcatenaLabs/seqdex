package covenant

import (
	"encoding/binary"
	"fmt"
)

// ReadOutputs reads a serialized Sequentia transaction's outputs as a FILL
// leaf reads them, from the raw bytes alone.
//
// It does not use go-elements, which cannot read every Sequentia transaction:
// an asset issuance carries a field Elements does not have (CAssetIssuance
// ends with its denomination, a compact size after the inflation keys), so a
// transaction with an issuance in it, an Arca round issuing its batch token
// among them, fails to parse there or parses into nonsense.
func ReadOutputs(raw []byte) ([]Output, error) {
	c := &cursor{b: raw}
	if err := c.skip(4); err != nil { // version
		return nil, err
	}
	if _, err := c.byte(); err != nil { // Elements witness flag
		return nil, err
	}
	nIn, err := c.varInt()
	if err != nil {
		return nil, err
	}
	if nIn > uint64(len(raw)) {
		return nil, fmt.Errorf("input count %d exceeds the transaction's %d bytes", nIn, len(raw))
	}
	for i := uint64(0); i < nIn; i++ {
		if err := c.skip(32); err != nil { // prevout hash
			return nil, err
		}
		index, err := c.uint32()
		if err != nil {
			return nil, err
		}
		if err := c.varSlice(nil); err != nil { // scriptSig
			return nil, err
		}
		if err := c.skip(4); err != nil { // sequence
			return nil, err
		}
		if index == 0xffffffff || index&(1<<31) == 0 {
			continue
		}
		// An issuance: blinding nonce, entropy, amount, inflation keys, and
		// Sequentia's denomination.
		if err := c.skip(64); err != nil {
			return nil, err
		}
		for j := 0; j < 2; j++ {
			if _, _, err := c.value(); err != nil {
				return nil, err
			}
		}
		if _, err := c.varInt(); err != nil {
			return nil, err
		}
	}
	nOut, err := c.varInt()
	if err != nil {
		return nil, err
	}
	if nOut > uint64(len(raw)) {
		return nil, fmt.Errorf("output count %d exceeds the transaction's %d bytes", nOut, len(raw))
	}
	outs := make([]Output, 0, nOut)
	for i := uint64(0); i < nOut; i++ {
		var o Output
		p, err := c.byte()
		if err != nil {
			return nil, err
		}
		switch p {
		case 1:
			b, err := c.take(32)
			if err != nil {
				return nil, err
			}
			copy(o.Asset[:], b)
			o.AssetExplicit = true
		case 10, 11:
			if err := c.skip(32); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("output %d: asset prefix %d", i, p)
		}
		v, explicit, err := c.value()
		if err != nil {
			return nil, fmt.Errorf("output %d: %w", i, err)
		}
		o.Value, o.ValueExplicit = v, explicit
		np, err := c.byte() // nonce: null, or a 33-byte commitment
		if err != nil {
			return nil, err
		}
		if np != 0 {
			if err := c.skip(32); err != nil {
				return nil, err
			}
			o.HasNonce = true
		}
		if err := c.varSlice(&o.Script); err != nil {
			return nil, err
		}
		outs = append(outs, o)
	}
	if err := c.skip(4); err != nil { // lock time; the witnesses after it are not read
		return nil, err
	}
	return outs, nil
}

type cursor struct {
	b   []byte
	pos int
}

func (c *cursor) take(n int) ([]byte, error) {
	if n < 0 || n > len(c.b)-c.pos {
		return nil, fmt.Errorf("transaction truncated at byte %d", c.pos)
	}
	b := c.b[c.pos : c.pos+n]
	c.pos += n
	return b, nil
}

func (c *cursor) skip(n int) error { _, err := c.take(n); return err }

func (c *cursor) byte() (byte, error) {
	b, err := c.take(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

func (c *cursor) uint32() (uint32, error) {
	b, err := c.take(4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b), nil
}

func (c *cursor) varInt() (uint64, error) {
	p, err := c.byte()
	if err != nil {
		return 0, err
	}
	n := 0
	switch p {
	case 0xfd:
		n = 2
	case 0xfe:
		n = 4
	case 0xff:
		n = 8
	default:
		return uint64(p), nil
	}
	b, err := c.take(n)
	if err != nil {
		return 0, err
	}
	var v uint64
	for i := n - 1; i >= 0; i-- {
		v = v<<8 | uint64(b[i])
	}
	return v, nil
}

func (c *cursor) varSlice(into *[]byte) error {
	n, err := c.varInt()
	if err != nil {
		return err
	}
	if n > uint64(len(c.b)) {
		return fmt.Errorf("length %d exceeds the transaction", n)
	}
	b, err := c.take(int(n))
	if err != nil {
		return err
	}
	if into != nil {
		*into = append([]byte(nil), b...)
	}
	return nil
}

// value reads a confidential value: null, explicit (8 bytes big-endian) or a
// 33-byte commitment.
func (c *cursor) value() (uint64, bool, error) {
	p, err := c.byte()
	if err != nil {
		return 0, false, err
	}
	switch p {
	case 0:
		return 0, false, nil
	case 1:
		b, err := c.take(8)
		if err != nil {
			return 0, false, err
		}
		return binary.BigEndian.Uint64(b), true, nil
	case 8, 9:
		return 0, false, c.skip(32)
	}
	return 0, false, fmt.Errorf("value prefix %d at byte %d", p, c.pos-1)
}
