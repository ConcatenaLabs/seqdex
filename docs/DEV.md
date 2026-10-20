# SeqDEX development guide

How to build, test, and run every part of this repo locally. Commands were
verified on Linux with Go 1.26 on 2026-07-08; binary names, flags and
subcommands were re-checked against the code on 2026-08-22. For what the parts
are and how they fit together, read the [README](../README.md) and
[daemon/README.md](../daemon/README.md) first ([ARCHITECTURE.md](ARCHITECTURE.md)
is a 2026-07-08 snapshot).

## Prerequisites

- A recent Go toolchain (Go 1.26 verified; the daemon's release build script
  uses `CGO_ENABLED=1`).
- For anything on-chain: a built Sequentia node, i.e. `sequentiad` and
  `sequentia-cli` from
  https://github.com/ConcatenaLabs/Sequentia (about an hour to
  build; see that repo's docs).
- Optional: [`buf`](https://buf.build) for proto codegen, Docker for the
  inherited upstream compose files, a SeqLN node for the Lightning rails
  (submarine, pure-LN, sub-asset).

## Build

The repo holds seven Go modules; the two that matter are `daemon/` and
`wallet/` (the others are the top-level `proto/gen` module and small
self-contained modules under `daemon/pkg` and `daemon/cmd/migration`). Build
the two service modules:

```sh
cd daemon && go build ./...
cd ../wallet && go build ./...
```

Binaries you will actually run:

```sh
cd daemon
go build -o ../bin/seqobd       ./cmd/seqobd        # order-book relay
go build -o ../bin/seqob-maker  ./cmd/seqob-maker   # order-book maker
go build -o ../bin/seqob-cli    ./cmd/seqob-cli     # order-book taker CLI
go build -o ../bin/seqob-octl   ./cmd/seqob-octl    # ocean account helper
go build -o ../bin/seqob-settler ./cmd/seqob-settler # passive covenant settler
go build -o ../bin/seqob-covenant ./cmd/seqob-covenant # covenant builder CLI
go build -o ../bin/tdexd        ./cmd/tdexd         # RFQ trade daemon
go build -o ../bin/tdex         ./cmd/tdex          # RFQ operator CLI
cd ../wallet
go build -o ../bin/seqdex-wallet ./cmd/oceand       # wallet daemon
```

(Upstream `make build` in `daemon/` builds `tdexd` via `scripts/build` with
CGO and `-ldflags="-s -w"`; the plain `go build` above is fine for
development.)

## Tests

Unit tests need no node and pass on a clean checkout:

```sh
cd daemon && go test ./internal/seqob/... ./pkg/xchain/... ./pkg/covenant/...
cd ../wallet && go test ./pkg/...
```

Notes:

- `daemon/pkg/xchain` contains real two-chain integration tests
  (`TestCrossChainSwap`, `TestReverseCrossChainSwap`, anchor negative tests).
  They automatically start and stop a two-chain regtest via
  `pkg/xchain/testdata/start-regtest.sh` when they can find the node binaries,
  and **skip** (not fail) otherwise. Point them at your node clone with
  `SEQUENTIA_REPO=/path/to/Sequentia` (the script expects
  `$SEQUENTIA_REPO/build-linux/src/sequentiad` and `sequentia-cli`, falling
  back to the legacy `elementsd` / `elements-cli` names), or at
  already-running nodes with the test-only env knobs `SEQDEX_XCHAIN_PARENT_RPC`
  / `SEQDEX_XCHAIN_SEQ_RPC`.
- The consensus-level covenant proofs (`test/regtest/feature_seqob_*.py`)
  run against a real node via the node repo's functional test framework; see
  [../test/regtest/README.md](../test/regtest/README.md).
- The submarine/Lightning integration tests additionally want `SEQLN_SOCK1` /
  `SEQLN_SOCK2` (two SeqLN `lightning-rpc` sockets) and skip without them.
- The upstream test suites under `wallet/internal/core/...` predate the
  Sequentia network adaptation and currently fail with "unknown network"; they
  are upstream Ocean tests, not a regression signal for the Sequentia paths.
- `daemon/test/e2e` (`make integrationtest`) is the inherited upstream TDEX
  docker loop against a Liquid regtest; it has not been adapted to Sequentia.

## The two-chain regtest

Everything cross-chain (and the most faithful same-chain testing) runs against
a pair of nodes: a parent Elements-mode node standing in for Bitcoin (the
anchor source) and an anchored Sequentia node. The committed harness brings
both up:

```sh
export SEQUENTIA_REPO=$HOME/Sequentia    # your node clone (build-linux/src/*)
daemon/pkg/xchain/testdata/start-regtest.sh up     # parent RPC :18000, seq RPC :18001
daemon/pkg/xchain/testdata/start-regtest.sh stop
daemon/pkg/xchain/testdata/start-regtest.sh clean  # stop + wipe /tmp/seqdex-xchain-regtest
```

It starts both nodes with `-signblockscript=51 -con_any_asset_fees=1
-blindedaddresses=0 ...`, wires the Sequentia node's anchoring to the parent
(`-con_bitcoin_anchor=1 -validateanchor=1 -mainchainrpc*`), and creates a
wallet `w` on each. Cross-chain flows against a real `bitcoind` (testnet4 or
Bitcoin regtest) are also supported by every binary via `-btc-chain`.

### Why Elements mode matters for the wallet

Plain `sequentiad -chain=regtest` runs in Bitcoin serialization mode, which the
go-elements transaction parsers the wallet relies on cannot decode. Real
Sequentia runs in Elements mode. For a faithful standalone node (without the
harness) use:

```sh
sequentiad -datadir=<dir> -server -daemon \
  -con_elementsmode=1 -signblockscript=51 -validatepegin=0 \
  -bech32_hrp=bcrt -blech32_hrp=bcrt -pubkeyprefix=111 -scriptprefix=196 -blindedprefix=4 \
  -rpcport=19996 -rpcuser=s -rpcpassword=s \
  -fallbackfee=0.0001 -acceptnonstdtxn=1 -txindex=1 -blockfilterindex=1
# create a descriptor wallet, mine ~110 blocks, then read getsidechaininfo.pegged_asset
```

These parameters match `wallet/pkg/seqnet.SequentiaRegtest` (bech32/blech32
`bcrt`, base58 111/196, confidential prefix 4). Note the harness's nodes run
with anchoring on, which is what exercises Sequentia's anchored PoS header
path; the wallet's scanner deliberately reads block and header structure via
JSON-RPC verbosity (not raw-header deserialization) because go-elements cannot
parse anchored/PoS headers.

## Run the wallet daemon

```sh
OCEAN_NETWORK=sequentia-regtest \
OCEAN_NATIVE_ASSET=<getsidechaininfo.pegged_asset hex> \
OCEAN_NODE_RPC_ADDR=http://s:s@127.0.0.1:19996 \
OCEAN_BLOCKCHAIN_SCANNER_TYPE=elements \
OCEAN_DB_TYPE=badger OCEAN_NO_TLS=true \
OCEAN_DATADIR=<datadir> \
./bin/seqdex-wallet
```

- `OCEAN_NODE_RPC_ADDR` must be a full URL with credentials.
- `OCEAN_NATIVE_ASSET` is required: Sequentia's policy asset is genesis-derived,
  read it from the node, never hardcode it.
- Leave `OCEAN_ESPLORA_URL` unset for node-RPC-only operation; the elements
  scanner polls the node tip every ~2s.
- Networks: `sequentia` | `sequentia-testnet` (default) | `sequentia-regtest`.
- Default gRPC port: `18000`.

Verified flow: GenSeed -> CreateWallet -> Unlock -> CreateAccount ->
DeriveAddresses -> fund the derived `bcrt1...` address from the node -> the
wallet syncs the UTXO/balance. Drive it with the `ocean` CLI
(`wallet/cmd/ocean`) or `seqob-octl`.

## Run the SeqOB loop (relay + maker + taker)

1. **Relay** (holds nothing, needs nothing but a port):

   ```sh
   ./bin/seqobd -listen :9955
   ```

2. **Prepare a maker account** on the running wallet daemon and fund the
   printed address from your node:

   ```sh
   ./bin/seqob-octl -ocean 127.0.0.1:18000 -account mm -action create
   ./bin/seqob-octl -ocean 127.0.0.1:18000 -account mm -action balance
   ```

3. **Maker**: posts a signed offer and serves lifts over the courier.

   ```sh
   ./bin/seqob-maker \
     -relay http://127.0.0.1:9955 -ocean 127.0.0.1:18000 -account mm \
     -node-rpc http://s:s@127.0.0.1:19996 \
     -base <GOLD hex> -quote <USDX hex> -side sell \
     -base-amount 100 -quote-amount 4500 \
     -min-anchor-depth 0 -confidential=true -expiry 1h
   ```

4. **Taker**: inspect the book, then lift.

   ```sh
   ./bin/seqob-cli book -relay http://127.0.0.1:9955 -base <GOLD hex> -quote <USDX hex>
   ./bin/seqob-cli lift -relay http://127.0.0.1:9955 \
     -base <GOLD hex> -quote <USDX hex> \
     -offer-id <id> -maker-pubkey <hex> -amount 100 \
     -esplora <esplora url> -taker-priv <32-byte hex> -taker-blinding <32-byte hex>
   ```

   Without `-esplora`/`-taker-priv`/`-taker-blinding` the lift runs in demo
   mode against an in-memory stub wallet (it exercises the courier handshake
   but settles nothing on-chain); with them the taker funds and broadcasts a
   real swap.

`seqob-cli post` can also place an offer without running a maker process, but
settling a lift requires the maker online (interactive co-signing; the relay
evicts a disconnected maker's offers).

### Cross-chain over the order book

Maker (sells an asset for real BTC, or the reverse; both need a Sequentia node
and a BTC-side node):

```sh
./bin/seqob-maker -mode cross \
  -relay http://127.0.0.1:9955 \
  -base <asset hex> -side sell \
  -base-amount 100 -quote-amount 15000 \
  -btc-rpc http://user:pass@127.0.0.1:48332 -btc-wallet w -btc-chain testnet4 \
  -xseq-rpc http://s:s@127.0.0.1:19996 -xseq-wallet w \
  -xstate-dir xmaker-sessions
# in -mode cross the pair is always base=asset, quote=the "BTC" sentinel;
# -base-amount is asset atoms, -quote-amount is BTC sats; no partial fills
```

Taker:

```sh
# buy the asset with BTC (forward; you hold the secret)
./bin/seqob-cli xlift    -relay ... -asset <hex> -offer-id ... -maker-pubkey ... \
  -btc-rpc ... -btc-wallet ... -btc-chain testnet4 -seq-rpc ... -seq-wallet ... -state-file lift.json
# sell the asset for BTC (reverse; the maker holds the secret)
./bin/seqob-cli xsell    ...same flags...
# refunds after an abort, once the CLTV passes
./bin/seqob-cli xrefund     -state-file lift.json -btc-rpc ... -btc-wallet ... -wait
./bin/seqob-cli xrefund-seq -state-file sell.json -seq-rpc ... -seq-wallet ... -wait
```

After a maker crash, `./bin/seqob-maker -resume -xstate-dir xmaker-sessions ...`
finishes every non-terminal session (claim or refund) and exits.

### Submarine (Lightning) over the order book

Requires a SeqLN/CLN node on the BTC side for each party
(https://github.com/ConcatenaLabs/seqln):

```sh
# maker: base = the asset, quote = the BTC sentinel
./bin/seqob-maker -mode lightning -ln-socket /path/to/lightning-rpc \
  -sub-anchor-depth 3 ...
# taker: sell the asset, receive BTC over LN
./bin/seqob-cli xsublift -ln-socket ... -relay ... -asset <hex> ...
# taker: buy the asset, pay BTC over LN (anchor-gate before paying)
./bin/seqob-cli xsubbuy  -ln-socket ... -min-anchor-depth 2 ...
```

### Pure Lightning and sub-asset over the order book

Pure-LN: both legs over Lightning, so each party needs a SeqLN node on
Sequentia (the asset leg) and one on Bitcoin (the BTC leg); nothing touches a
chain and there is no anchor wait.

```sh
# maker: base = the asset, quote = the BTC sentinel (or -quote-asset <hex> for asset-for-asset)
./bin/seqob-maker -mode pureln -relay ... -base <asset hex> -side sell \
  -base-amount 100 -quote-amount 15000 \
  -asset-ln-socket /path/to/seq/lightning-rpc -ln-socket /path/to/btc/lightning-rpc
# taker
./bin/seqob-cli xpln -relay ... -side buy -asset <hex> -offer-id ... -maker-pubkey ... \
  -asset-ln-socket ... -ln-socket ...
```

Sub-asset (the submarine's mirror: the asset over Lightning, BTC as an
on-chain HTLC):

```sh
# maker sells the asset over LN against on-chain BTC
./bin/seqob-maker -mode subasset -relay ... -base <asset hex> \
  -base-amount 100 -quote-amount 15000 \
  -asset-ln-socket ... -btc-rpc ... -btc-wallet ... -btc-chain testnet4
# taker pays BTC on-chain, receives the asset over LN
./bin/seqob-cli xsubas -relay ... -asset <hex> -offer-id ... -maker-pubkey ... \
  -btc-rpc ... -btc-wallet ... -btc-chain testnet4 -asset-ln-socket ... -state-file subas.json
# the reverse direction is -mode subasset-sell / seqob-cli xsubas-sell (+ xsubas-claim-btc)
```

### Covenant resting orders (passive)

A covenant order is funded on-chain, so the maker needs nothing running after
placement. The end-to-end proofs are the regtest scripts in `test/regtest/`
(`feature_seqob_covenant_fill.py`, `feature_seqob_joint_covenant.py`,
`feature_seqob_matcher_covenant.py`, `feature_seqob_watcher.py`,
`feature_seqob_bridge.py`, `feature_seqob_multi_fill.py`); they drive the
production Go builders through the CLIs:

```sh
./bin/seqob-covenant derive -a <A> -b <B> -num N -den D -minlot M -prog P -expiry E -makerx X
./bin/seqob-covenant fill   ...derive flags... -locked L -filled F -k K
./bin/seqob-covenant filltx < spec.json        # several orders, order k at input k
./bin/seqob-covenant check  ...derive flags... -tx HEX -k K -locked L
echo '<cross json>' | ./bin/seqob-settler plan
./bin/seqobd -listen :9955 -node-rpc 127.0.0.1:19996 -node-rpc-user <user> -node-rpc-pass <pass>
#   ^ the relay runs the covenant chain-watcher when given -node-rpc
```

## Run the RFQ loop (tdexd)

```sh
# wallet daemon up first (see above), then:
SEQDEX_WALLET_ADDR=127.0.0.1:18000 \
SEQDEX_NODE_RPC=http://s:s@127.0.0.1:19996 \
SEQDEX_NO_MACAROONS=true SEQDEX_NO_OPERATOR_TLS=true \
./bin/tdexd
```

Then initialize and drive it with the helpers (regtest) or the `tdex` CLI:
`seqdex-initwallet` (create), `seqdex-unlock` (unlock; this starts the Trade
interface on `:9945`), `seqdex-market` (create/fund/open a market),
`seqdex-taker` (perform a same-chain swap). tdexd serves same-chain trades
only; for cross-chain runs use the order book ("Cross-chain over the order
book" above).

## Proto codegen

The standalone contract copy under `proto/` regenerates with buf:

```sh
cd proto && buf dep update && buf generate   # -> proto/gen/go
```

The daemon and wallet vendor their own generated stubs under
`daemon/api-spec/protobuf/gen` and `wallet/api-spec/protobuf/gen`
(`make proto` in each directory; requires buf).

## Contributing

- Branch from `main`, open PRs against `main`.
- Go code is gofmt-formatted (`make lint` in `daemon/` runs golangci-lint).
- Module import paths are `github.com/aejkcs50/seqdex/...` (a historical
  GitHub username; the modules are self-contained so this only affects import
  strings, not fetching).
- Never commit keys, seeds, RPC credentials, or wallet files; the repo is
  public.
