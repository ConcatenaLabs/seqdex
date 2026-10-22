package covenant

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/bits"
)

// A Simplicity template whose program sits beside a data leaf of its terms:
//
//	P2TR(NUMS, TapBranch(TapLeaf_0xbe(cmr), H_TapData(terms)))
//
// seqdex/order, seqdex/pool and seqdex/round-order all have this shape, so an
// instance's address is the template's root plus its terms.

// deriveDataTree computes the output of the program cmr beside the data leaf terms.
func deriveDataTree(cmr [32]byte, terms []byte) (*SimTree, error) {
	prog := taggedHash("TapLeaf/elements", []byte{simLeafVersion, 0x20}, cmr[:])
	data := taggedHash("TapData", terms)
	lo, hi := prog, data
	if bytes.Compare(hi[:], lo[:]) < 0 {
		lo, hi = hi, lo
	}
	root := taggedHash("TapBranch/elements", lo[:], hi[:])
	key, parity, err := tweakOutputKey(NUMS, root)
	if err != nil {
		return nil, err
	}
	cb := make([]byte, 0, 65)
	cb = append(cb, simLeafVersion|parity)
	cb = append(cb, NUMS[:]...)
	cb = append(cb, data[:]...)
	return &SimTree{
		Terms: terms, ProgramLeafHash: prog, TermsLeafHash: data, MerkleRoot: root,
		OutputKey: key, Parity: parity, ScriptPubKey: append([]byte{op1, 0x20}, key[:]...), ControlBlock: cb,
	}, nil
}

// simTemplate is a vendored template's fills.json: its root and the pruned
// program of each path.
type simTemplate struct {
	TemplateHash string
	CMR          [32]byte
	Programs     map[string][]byte
}

func loadFills(name string, raw []byte, paths ...string) simTemplate {
	var f simFillsFile
	if err := json.Unmarshal(raw, &f); err != nil {
		panic(name + "/fills.json: " + err.Error())
	}
	t := simTemplate{TemplateHash: f.TemplateHash, CMR: mustHex32(f.CMR), Programs: map[string][]byte{}}
	for _, p := range paths {
		b, err := hex.DecodeString(f.Paths[p].Program)
		if err != nil || len(b) == 0 || len(b) != f.Paths[p].ProgramBytes {
			panic(name + "/fills.json: path " + p)
		}
		t.Programs[p] = b
	}
	return t
}

// bitWriter packs a Simplicity witness: values most significant bit first,
// padded with zero bits to a whole byte.
type bitWriter struct {
	b []byte
	n uint
}

func (w *bitWriter) bit(v bool) {
	if w.n%8 == 0 {
		w.b = append(w.b, 0)
	}
	if v {
		w.b[len(w.b)-1] |= 0x80 >> (w.n % 8)
	}
	w.n++
}

func (w *bitWriter) bytes(p []byte) {
	for _, x := range p {
		for i := 7; i >= 0; i-- {
			w.bit(x>>uint(i)&1 == 1)
		}
	}
}

func (w *bitWriter) u64(v uint64) {
	for i := 63; i >= 0; i-- {
		w.bit(v>>uint(i)&1 == 1)
	}
}

// mulLE reports a*b <= c*d, at 128 bits.
func mulLE(a, b, c, d uint64) bool {
	h1, l1 := mul64(a, b)
	h2, l2 := mul64(c, d)
	return h1 < h2 || (h1 == h2 && l1 <= l2)
}

// ceilMulDiv is ceil(a*b/c) when it fits 64 bits.
func ceilMulDiv(a, b, c uint64) (uint64, error) {
	if c == 0 {
		return 0, fmt.Errorf("division by zero")
	}
	h, l := mul64(a, b)
	q, r, err := div128(h, l, c)
	if err != nil {
		return 0, err
	}
	if r != 0 {
		if q == ^uint64(0) {
			return 0, fmt.Errorf("ceil(%d*%d/%d) does not fit 64 bits", a, b, c)
		}
		q++
	}
	return q, nil
}

// floorMulDiv is floor(a*b/c) when it fits 64 bits.
func floorMulDiv(a, b, c uint64) (uint64, error) {
	if c == 0 {
		return 0, fmt.Errorf("division by zero")
	}
	h, l := mul64(a, b)
	q, _, err := div128(h, l, c)
	return q, err
}

func mul64(a, b uint64) (hi, lo uint64) { return bits.Mul64(a, b) }

// div128 divides the 128-bit (hi, lo) by c, refusing a quotient beyond 64 bits.
func div128(hi, lo, c uint64) (q, r uint64, err error) {
	if hi >= c {
		return 0, 0, fmt.Errorf("quotient does not fit 64 bits")
	}
	q, r = bits.Div64(hi, lo, c)
	return q, r, nil
}
