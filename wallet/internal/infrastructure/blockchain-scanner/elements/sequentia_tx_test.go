package elements_scanner

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vulpemventures/go-elements/elementsutil"
	"github.com/vulpemventures/go-elements/transaction"
)

// Both vectors come from the Sequentia testnet. Each carries one issuance,
// so go-elements alone cannot decode it.
const (
	// genesis block, tx 1: no witness flag, issuance with explicit amount
	// and no inflation keys.
	genesisIssuanceTxid = "3e14089cfb44a55f2719c6170179ce5788d925e96ed723050138ca14efc127c0"
	genesisIssuanceHex  = "01000000000129d75d258e03dc8758382d6bd793414670860125c848861224d1e8cc0e2899c60000008000ffffffff0000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000001008e1bc9bf04000000080201403eae2f5587864d10e2b0386c6ec31c1019834d431ed31c93e15309cfcaecc80100005af3107a400000be03e40940b27530817ffa1dc89051ab64a40847935413863e646b7552caa0ca16e1eb4306cc4e74417a77ec366d4642e12a14c0d5afcd57754c60a6d45071d685c1f25df3b7a7c44f6536ce2f5f26feccff78cf0537128a203bda1c95d3cf6829220464128fb147df882f01aa1cdea93428a9655a8f52602f01e3c53512220cbba643e1020b6e3fb12c3eba5e0f3f44098549769daf3c44fa141e7521028f88c9848c86c311934a5939ceb98408975055fc7ee6b40b479969665afe0e6bac01403eae2f5587864d10e2b0386c6ec31c1019834d431ed31c93e15309cfcaecc801008dc0d6ae89c00000160014087d5935d6f0a9e7fb95e29389d34c4ac166611400000000"

	// block 92, tx 1: segwit spend issuing 1,000,000 units plus 1 token.
	segwitIssuanceTxid = "00d17e2595ab625792353ce31ea703f73647b8d123e9e5814001e9b744c119c0"
	segwitIssuanceHex  = "0200000001015f62008c12163c859a3643c82f96e02aa26bf2428d56bd1bb8104a15b03d90be0900008000fdffffff000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000100005af3107a4000010000000005f5e100080401a171ccd2a7aa7bb82797145d25e1df951031c63ad9877f8d9fb59d2192910f3a0100005af3107a40000016001429d622cf6399be74e6d04cc45ccd53409fe5c6db01403eae2f5587864d10e2b0386c6ec31c1019834d431ed31c93e15309cfcaecc801008d97e94d846937001600142dbb421fc1f06a3abd2094e5b2576f6ce910b48501555c24c643c7df454e7179db0fdec0e9b2ae330b26f303539eaa80bb68e2ba14010000000005f5e100001600140e3c65de81986fa69b15bd3b30dce98b445a70c901403eae2f5587864d10e2b0386c6ec31c1019834d431ed31c93e15309cfcaecc801000000000000002900005a000000000002473044022057e0de4a1a9e3237f87d23d9b2e2593cc0d496eb3a11e813e7198d5a29e4ded502201008261004a37a370ed9db29671365c301220d4a9acc927b646f20c21ad6218e0121028f88c9848c86c311934a5939ceb98408975055fc7ee6b40b479969665afe0e6b000000000000000000"
)

func TestDecodeSequentiaTxWithIssuance(t *testing.T) {
	t.Run("go-elements alone fails", func(t *testing.T) {
		_, err := transaction.NewTxFromHex(segwitIssuanceHex)
		require.Error(t, err)
	})

	t.Run("segwit issuance", func(t *testing.T) {
		tx, err := decodeSequentiaTx(segwitIssuanceHex)
		require.NoError(t, err)
		require.Len(t, tx.Inputs, 1)
		require.Len(t, tx.Outputs, 4)

		iss := tx.Inputs[0].Issuance
		require.NotNil(t, iss)
		amount, err := elementsutil.ValueFromBytes(iss.AssetAmount)
		require.NoError(t, err)
		require.EqualValues(t, 100_000_000_000_000, amount)
		token, err := elementsutil.ValueFromBytes(iss.TokenAmount)
		require.NoError(t, err)
		require.EqualValues(t, 100_000_000, token)

		require.Equal(t,
			"001429d622cf6399be74e6d04cc45ccd53409fe5c6db",
			hex.EncodeToString(tx.Outputs[0].Script),
		)
		require.Len(t, tx.Inputs[0].Witness, 2)
	})

	t.Run("genesis issuance", func(t *testing.T) {
		tx, err := decodeSequentiaTx(genesisIssuanceHex)
		require.NoError(t, err)
		require.Len(t, tx.Inputs, 1)
		require.Len(t, tx.Outputs, 2)
		require.Equal(t,
			"0014087d5935d6f0a9e7fb95e29389d34c4ac1666114",
			hex.EncodeToString(tx.Outputs[1].Script),
		)
	})
}

func TestNetworkTxRestoresTxid(t *testing.T) {
	tx, err := decodeSequentiaTx(segwitIssuanceHex)
	require.NoError(t, err)

	// The decoded tx serializes without the denomination, so its own hash
	// is not the network txid.
	require.NotEqual(t, segwitIssuanceTxid, tx.TxHash().String())

	rememberNetworkTx(tx, segwitIssuanceTxid, segwitIssuanceHex)
	txid, txHex := networkTx(tx)
	require.Equal(t, segwitIssuanceTxid, txid)
	require.Equal(t, segwitIssuanceHex, txHex)
}

func TestDecodeSequentiaTxWithoutIssuance(t *testing.T) {
	plain := transaction.NewTx(2)
	plain.AddInput(transaction.NewTxInput(make([]byte, 32), 1))
	plain.AddOutput(transaction.NewTxOutput(
		make([]byte, 33), []byte{1, 0, 0, 0, 0, 0, 0, 0, 1}, []byte{0x51},
	))
	plainHex, err := plain.ToHex()
	require.NoError(t, err)

	raw, _ := hex.DecodeString(plainHex)
	stripped, err := stripIssuanceDenominations(raw)
	require.NoError(t, err)
	require.Equal(t, raw, stripped)

	tx, err := decodeSequentiaTx(plainHex)
	require.NoError(t, err)
	require.Equal(t, plain.TxHash(), tx.TxHash())

	// Without an issuance the hashes agree and nothing is remembered.
	txid, _ := networkTx(tx)
	require.Equal(t, plain.TxHash().String(), txid)
}

func TestDecodeSequentiaTxDoesNotPanic(t *testing.T) {
	// One input without issuance, no outputs, then a witness whose first
	// length is 2^64-1: go-elements panics in makeslice on it.
	bad := "02000000" + "01" + "01" +
		"0000000000000000000000000000000000000000000000000000000000000000" +
		"01000000" + "00" + "ffffffff" +
		"00" + "00000000" +
		"ffffffffffffffffff"

	require.Panics(t, func() { _, _ = transaction.NewTxFromHex(bad) })

	_, err := decodeSequentiaTx(bad)
	require.Error(t, err)
}

func TestDecodeSequentiaTxTruncated(t *testing.T) {
	// Cut inside the issuance, before the denomination byte.
	_, err := decodeSequentiaTx(segwitIssuanceHex[:200])
	require.Error(t, err)
}
