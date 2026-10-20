# SeqDEX daemon module

The Go module holding everything server- and client-side for SeqDEX except the
wallet daemon: the SeqOB order-book relay and its maker/taker/settler programs,
the covenant builder, the RFQ trade daemon (forked from
[tdex-daemon](https://github.com/tdex-network/tdex-daemon)), and the
cross-chain HTLC engine. The wallet daemon it settles through lives in
[`../wallet`](../wallet). Design: [../docs/ARCHITECTURE.md](../docs/ARCHITECTURE.md)
(a 2026-07-08 snapshot; this file is current as of 2026-08-22);
runbooks: [../docs/DEV.md](../docs/DEV.md).

Everything here is testnet software; nothing in this module ever holds user
funds (the RFQ daemon holds only its operator's own market-making liquidity,
via the wallet daemon, and `seqob-bridge` only its own fronted inventory).

## Binaries

### `cmd/seqobd`: the order-book relay

Non-custodial: stores signed offers, serves the per-pair book, matches
crossing orders, couriers opaque end-to-end-encrypted swap-session frames. No
wallet, no keys, no funds, never decrypts.

```
seqobd [flags]
  -listen            HTTP listen address        (default ":9955", env SEQOB_LISTEN)
  -node-rpc          read-only Sequentia node RPC host:port for the covenant
                     chain-watcher; empty disables the watcher
                                                (env SEQOB_NODE_RPC)
  -node-rpc-user / -node-rpc-pass   node JSON-RPC credentials
                                                (env SEQOB_NODE_RPC_USER / _PASS)
  -watch-interval    chain-watcher reconcile interval (default 5s)
  -session-deadline  lift co-sign deadline      (default 2m)
  -xsession-deadline courier deadline for cross-chain lift sessions
                     (default 3h; they span a real parent-chain confirmation)
  -expiry-sweep      offer expiry sweep interval        (default 15s)
  -session-sweep     session deadline sweep interval    (default 10s)
  -min-expiry / -max-expiry   allowed offer expiry horizon (30s .. 168h)
  -offers-per-min / -offers-per-min-ip   rate limits (60 per maker key, 120 per IP)
  -trade-log         append-only JSONL trade log so last_price, trades and
                     candles survive a restart (env SEQOB_TRADE_LOG; empty =
                     in-memory only)
  -interactive-cap   soft per-maker committed-capital cap per offered asset
                     (atoms) for interactive offers; past it an offer is
                     flagged demoted (phantom depth); 0 disables
  -fill-anchor-depth Bitcoin blocks that must bury a covenant fill's anchor,
                     its block certified, before the relay records the fill
                     final (default 2; an offer's min_anchor_depth raises it)
  -arca-operator     comma-separated base URLs of Arca operators; a covenant
                     fill made by one of their rounds is recorded as that
                     round's (env SEQOB_ARCA_OPERATORS)
```

HTTP surface (bodies are protojson of `api-spec/protobuf/seqob/v1`):

```
POST /v1/offers                             submit a signed Offer
POST /v1/offers/cancel                      submit a signed OfferCancel
GET  /v1/offers?maker_pubkey=<hex>          a maker's own orders
GET  /v1/markets                            market summaries
GET  /v1/market/{base}/{quote}/orderbook    per-pair snapshot
GET  /v1/market/{base}/{quote}/trades       recent trades
GET  /v1/market/{base}/{quote}/candles      OHLCV from the trades
GET  /v1/market/{base}/{quote}/fills        covenant fill records (JSON)
POST /v1/lift                               open a lift session
GET  /v1/ws                                 WebSocket (To/From envelope): book
                                            subscriptions, offer submission,
                                            lift and match notifications,
                                            opaque SwapMsg courier
```

Live instances: `https://sequentiatestnet.com/seqob/v1/...` (on-chain rails),
`/seqob-pln/v1/...` (Lightning rails), `/seqob-conf/v1/...` (confidential
markets); one `seqobd` process per mount.

### `cmd/seqob-maker`: the maker participant

Posts one signed resting offer and serves lifts until stopped. Anyone can run
one; the relay grants makers no special standing. Six settlement modes:

- `-mode samechain` (default): asset-for-asset atomic swap, settled through the
  Ocean wallet's blind-aware `CompleteSwap`. Needs `-ocean` + `-account` (the
  account holding the offered asset) and `-node-rpc` for the open fee market.
  `-confidential=false` (default: Sequentia is transparent by default) settles
  explicit; `-confidential=true` publishes a blinding pubkey and settles blinded.
- `-mode cross`: BTC-for-asset on-chain HTLC swaps. Funds legs from node
  wallets directly: needs `-btc-rpc`/`-btc-wallet`/`-btc-chain`
  (testnet4|regtest) and `-xseq-rpc`/`-xseq-wallet`. Pair is always
  base = the asset (`-base`), quote = the `"BTC"` sentinel; `-base-amount` is
  asset atoms, `-quote-amount` BTC sats; no partial fills. Per-lift session
  state persists under `-xstate-dir`; `-resume` finishes every non-terminal
  session (claim or refund) after a restart, then exits. Safety dials:
  `-min-btc-conf` (default 1), `-btc-locktime-delta` (T_btc, default 100
  parent blocks), `-seq-locktime-delta` (T_seq, default 240 Sequentia blocks),
  `-btc-fee-rate`, `-spend-fee`.
- `-mode lightning`: submarine swaps (asset on-chain against BTC over
  Lightning). Needs `-ln-socket` (a SeqLN/CLN `lightning-rpc` on Bitcoin) plus
  `-xseq-rpc`/`-xseq-wallet`. `-side buy` serves the NORMAL direction (taker
  sells the asset, maker pays the taker's invoice), `-side sell` the REVERSE
  (maker locks the asset, taker pays the maker's invoice).
  `-sub-anchor-depth` (default 3, floor 2) is the Bitcoin-anchor depth the
  maker requires on the taker's Sequentia funding before taking the
  irreversible Lightning action.
- `-mode pureln`: both legs over Lightning, no on-chain leg and no anchor
  wait. Needs `-ln-socket` (BTC leg) and `-asset-ln-socket` (the maker's
  SeqLN-on-Sequentia socket, asset leg). `-quote-asset` advertises an
  asset-for-asset market instead of the BTC sentinel and `-btc-asset` routes
  the counter leg over that asset; `-hold-timeout` (default 2m) bounds the
  wait for the taker's hold invoice.
- `-mode subasset` / `-mode subasset-sell`: the submarine's mirror, the asset
  over Lightning against an on-chain BTC HTLC. `subasset`: the taker pays BTC
  on-chain and receives the asset over LN; `subasset-sell`: the reverse. Need
  `-asset-ln-socket` plus `-btc-rpc`/`-btc-wallet`/`-btc-chain`; with
  `-quote-asset` the on-chain leg becomes an HTLC on that Sequentia asset
  (via `-xseq-rpc`/`-xseq-wallet`) and no bitcoind is involved.

`-requote` (cross, lightning, pureln, sub-asset) re-posts a fresh offer under
the same id after each settled fill instead of exiting.

Common flags: `-relay`, `-maker-priv` (32-byte hex identity/E2E key, generated
if empty), `-base`, `-quote`, `-side sell|buy`, `-base-amount`,
`-quote-amount`, `-fee-asset` (hint, any-asset fee market),
`-min-anchor-depth` (Bitcoin-anchor confirmations before the order is FILLED;
default 0 = 0-conf tolerant), `-expiry`, `-offer-id`, `-msats-per-byte`.

### `cmd/seqob-cli`: the taker (and ad-hoc maker) CLI

```
seqob-cli post         post a signed offer (no maker process; lifts need one)
seqob-cli book         print a market's order book
seqob-cli lift         lift a same-chain offer (real settlement with
                       -esplora -taker-priv -taker-blinding; stub-wallet
                       demo mode without them)
seqob-cli xlift        buy an asset with on-chain BTC (forward cross-chain)
seqob-cli xsell        sell an asset for on-chain BTC (reverse cross-chain)
seqob-cli xrefund      recover the BTC leg of an aborted xlift after T_btc
seqob-cli xrefund-seq  recover the asset leg of an aborted xsell after T_seq
seqob-cli xsublift     sell an asset for BTC over Lightning (normal submarine)
seqob-cli xsubbuy      buy an asset with BTC over Lightning (reverse submarine;
                       anchor-gates the maker's HTLC before paying)
seqob-cli xpln         trade an asset against BTC with both legs over
                       Lightning (pure-LN; -side buy|sell, -asset-ln-socket,
                       -ln-socket)
seqob-cli xsubas       buy an asset by paying BTC on-chain and receiving it
                       over Lightning (sub-asset; -taker-ln-node-id for a
                       non-custodial hold-invoice buy)
seqob-cli xsubas-refund      recover the BTC HTLC of an aborted xsubas after T_btc
seqob-cli xsubas-sell        sell an asset over Lightning for on-chain BTC
seqob-cli xsubas-claim-btc   claim the BTC HTLC of an xsubas-sell with the preimage
seqob-cli xsubas-refund-btc  LSP side: reclaim a payer-bridge BTC HTLC after T_btc
seqob-cli xsubas-htlc-spend-status
                       read the on-chain fate of a BTC HTLC (unspent, claimed
                       with the preimage, or refunded); fails closed
seqob-cli xsubas-node-caps   assert the BTC node is unpruned with a synced
                       txindex before running payer bridges
seqob-cli xhtlc-observe      report an HTLC outpoint's spend on either chain
                       (extracting the preimage when given the hash)
seqob-cli xfund-seq    fund an on-chain Sequentia asset HTLC
seqob-cli xseq-refund  reclaim an asset HTLC by outpoint after T_seq
seqob-cli keygen       print a fresh 32-byte hex key and its pubkey
```

Run any subcommand without flags for its exact flag list. Cross-chain
subcommands persist their session (secret, keys, legs, locktimes) to
`-state-file` before funding anything, so refunds survive a crash.

### `cmd/seqob-settler`: the passive-CLOB settler

Always-online, keyless, fundless settler for two covenant-funded resting
orders whose makers are both offline: the relay matches them, the settler
assembles the single joint FILL transaction, funds its fee and broadcasts.
Both covenants' FILL leaves enforce every maker credit, so it can only build a
conforming transaction or none. `plan` (JSON cross on stdin) prints the
enforced recipe; `run` is the loop against a relay and a node.

### `cmd/seqob-watcher`: the covenant chain-watcher

Reconciles the relay's covenant book to the node's current tip and keeps the
record of every covenant fill. In production the same core runs inside
`seqobd` (`-node-rpc`); this binary exposes its classifier standalone:
`classify` (JSON state on stdin) and `run`.

- A transaction that spends several covenant orders is several fills, one per
  order, `k` its input index. Each is recorded from the transaction's outputs
  as the order's FILL leaf reads them: the maker's credit at `2k`, and at
  `2k+1` the order resting again (a partial fill) or the gap that stands there
  (a full fill). The fill's trade is recorded with it, txid attached.
- The taker is whoever the filling transaction's other outputs pay, which the
  record does not attribute, unless the transaction is a round of an Arca
  operator the relay follows (`-arca-operator`): the operator lists it among
  its rounds, runs on the relay's chain, and each batch output it names pays
  the root of the tree it publishes. The taker's side of such a fill is a leaf
  in the round's batch, so no output of the round pays the taker, and the
  record names the operator's key, the round and its batch outputs instead.
- After a partial fill the order rests again at `(txid, 2k+1)` with its terms,
  and so its signature, unchanged: the covenant outpoint is outside the
  signed bytes. After a full fill the order is held, hidden from matching,
  and leaves the book (FILLED) once the fill is final.
- A fill is final once its block is quorum-certified (it, or a block above
  it) and its Bitcoin anchor is buried `-fill-anchor-depth` blocks (the
  offer's `min_anchor_depth` when that is more), with the node reporting its
  tip's anchor checked (`getanchorstatus` "ok"). A chain without Bitcoin
  anchors, such as a test chain, counts its own blocks above the fill. The
  relay's node needs `-txindex`, and `-validateanchor` on an anchored chain.
- A fill whose block leaves the active chain, final or not (an anchor reorg,
  a round replaced by another), is undone: its record says so and why, its
  trade is retracted, and the order rests again where it rested before it,
  with every later fill of its rest undone too. Whatever then spends that
  outpoint (the same transaction mined again, or a replacement round filling
  a subset of the orders at other `k`) is recorded as a new fill.
- A spend through the REFUND leaf (or any path but FILL) is the maker taking
  the order back: no fill and no trade; the order leaves the book (EXPIRED)
  once that spend is final.
- Orders whose funding UTXO is gone at the tip with no spender are removed
  (ghosts), unconfirmed spends are held pending, and fills that never
  confirmed are re-opened.

### `cmd/seqob-bridge`: the cross-rail bridge

Settles a covenant order against a Lightning order under one preimage when the
matcher crosses them: fills the on-chain covenant and delivers/collects the
Lightning leg through the submarine/pure-LN orchestrator. Unlike every other
seqob component it holds its own inventory and an LN node; the only capital at
risk is its own fronted inventory, bounded by the per-offer 0-conf cap.
`plan` and `run` as for the settler.

### `cmd/seqob-covenant`: the covenant builder CLI

`derive` prints a resting order's leaves, merkle data, output key and
scriptPubKey; `fill` prints the permissionless FILL spend of an order at input
`k` (credit index `2k`, remainder index `2k+1`, witness, control block);
`filltx` lays out a transaction filling several orders, order `k` at input
`k`, from a JSON spec on stdin, and runs every order's leaf over it; `check`
judges a transaction as an order's FILL leaf at input `k` would. The scripts
are byte-identical to the Python builder in `../test/regtest`, pinned by
`pkg/covenant/leaf_test.go`; the multi-order layout is pinned by
`pkg/covenant/testdata/fill_vectors.json`, which the web wallet's
`covenant.js` reproduces byte for byte.

### `cmd/seqob-relaycli`: relay scripting client

`post` signs an offer and POSTs it (the stateless maker path); `take` signs,
submits over WebSocket and waits for the matcher's match. Used by the regtest
matcher proof.

### `cmd/seqob-octl`: maker account helper

Read-mostly Ocean helper: `-action balance | address | create` against
`-ocean <addr> -account <name>`. No keys, no signing.

### `cmd/tdexd`: the RFQ trade daemon

The forked tdex-daemon. It serves same-chain trades only; cross-chain lives in
`pkg/xchain` behind the SeqOB maker, CLI, settler and bridge (the RFQ
`XchainService` was removed on 2026-07-29). Configuration is env-only with
prefix `SEQDEX_` (upstream used `TDEX_`); the main keys (see
`internal/config/config.go` for all):

| Env | Meaning | Default |
|---|---|---|
| `SEQDEX_WALLET_ADDR` | ocean wallet gRPC endpoint | - |
| `SEQDEX_TRADE_LISTENING_PORT` | Trade interface (gRPC + grpc-web + REST) | `9945` |
| `SEQDEX_OPERATOR_LISTENING_PORT` | Operator interface | `9000` |
| `SEQDEX_NODE_RPC` | Sequentia node RPC URL for `getfeeexchangerates` (any-asset network fees); unset = legacy native-asset fee path | - |
| `SEQDEX_NO_MACAROONS`, `SEQDEX_NO_OPERATOR_TLS` | disable auth/TLS (dev) | `false` |
| `SEQDEX_DATADIR`, `SEQDEX_DB_TYPE`, `SEQDEX_LOG_LEVEL` | state and logging | `~/.seqdex-daemon`, `badger`, `4` |

Interfaces served:

- **Trade** (`:9945`, TLS optional, permissive CORS): gRPC
  `seqdex.v1.TradeService` plus grpc-web and a grpc-gateway REST mapping:
  `POST /v1/markets`, `/v1/market/balance`, `/v1/market/price`,
  `/v1/trade/preview`, `/v1/trade/propose`, `/v1/trade/complete`. The Trade
  interface starts after the wallet is unlocked.
- **Operator** (`:9000`, macaroons unless disabled): wallet init/unlock, market
  lifecycle, deposits, withdrawals, fee config (protos under
  `api-spec/protobuf/tdex-daemon/v2`). Drive it with `cmd/tdex` or the
  `seqdex-initwallet` / `seqdex-unlock` / `seqdex-market` helpers.

The REST gateway is not publicly exposed on the testnet.

### Demos and regtest helpers

- `cmd/seqdex-xchain-swapdemo`: runs the raw HTLC mechanism end to end against
  the two-chain regtest, including the negative test (a Sequentia leg with an
  insufficient anchor height is rejected). Despite its name it exercises
  `pkg/xchain`, not the retired RFQ service.
- `cmd/seqdex-taker`, `cmd/seqdex-market`, `cmd/seqdex-initwallet`,
  `cmd/seqdex-unlock`: throwaway regtest helpers for the RFQ loop.

## Package map

```
internal/seqob/offer       offer/cancel canonical bytes, signing, verification,
                           the BTC sentinel, direction consistency
internal/seqob/offerstore  the book: keys, statuses, partial fills, anchor-aware
                           FILLED, reopen hook, expiry sweeper, trade log
internal/seqob/validator   admission: signature/schema/amounts/expiry checks,
                           rate limits, replay gate (liveness probe = stub)
internal/seqob/matcher     continuous matching of crossing orders, including
                           covenant-vs-covenant and cross-rail matches
internal/seqob/session     lift-session router: peer binding, opaque courier,
                           deadlines (reorg watcher for interactive orders = stub)
internal/seqob/api         REST + WS server over the To/From envelope
internal/seqob/client      taker/maker drivers: same-chain (driver.go),
                           cross-chain (xdriver*.go), submarine
                           (xdriver_submarine*.go), pure-LN (xdriver_pureln.go),
                           sub-asset (xdriver_subasset*.go), E2E crypter, live
                           wallet backend over Ocean
internal/seqob/covfill     covenant fill construction for takers (one or several orders)
internal/seqob/settler     joint settlement of two covenant orders
internal/seqob/watcher     covenant book reconciliation against the chain
internal/seqob/bridge      cross-rail (covenant vs Lightning) settlement
internal/seqob/reorg       Bitcoin-anchor reorg detection helpers
pkg/covenant               the tapscript covenant leaf builder (NUMS key,
                           FILL/REFUND leaves, output key, fill planning)
pkg/xchain                 the HTLC engine: legs (elements/bitcoin/lightning),
                           orchestrator, VerifySeqLegSafe anchor gate,
                           submarine, pure-LN and sub-asset swaps, regtest
                           harness (testdata/)
internal/core, internal/infrastructure, pkg/trade, pkg/swap, ...
                           the inherited tdex-daemon core (application services,
                           ocean-wallet port, taker SDK), adapted for Sequentia
                           (seqnet params, runtime policy asset, any-asset fees)
pkg/seqnet                 Sequentia network parameters + address handling
api-spec/protobuf          authoritative contracts: seqdex/v1, seqob/v1,
                           tdex-daemon/v1+v2, generated Go under gen/
```

## Build and test

```sh
go build ./...                                   # everything
make build                                       # tdexd via scripts/build (CGO, stripped)
go test ./internal/seqob/... ./pkg/xchain/... ./pkg/covenant/...
                                                 # unit + (auto-skipping) integration
make lint                                        # golangci-lint
```

The `pkg/xchain` integration tests start their own two-chain regtest via
`pkg/xchain/testdata/start-regtest.sh` when `SEQUENTIA_REPO` points at a built
node clone (or use running nodes via the test-only env knobs
`SEQDEX_XCHAIN_PARENT_RPC` / `SEQDEX_XCHAIN_SEQ_RPC`); otherwise they skip.
The consensus-level covenant proofs live in `../test/regtest`. See
[../docs/DEV.md](../docs/DEV.md).

## Upstream

This module began as tdex-daemon (MIT); upstream docs live at
https://dev.tdex.network/. The `internal/seqob`, `pkg/covenant` and
`pkg/xchain` trees and the `seqob*`/`seqdex-*` commands are new here;
`../NOTICE` retains the upstream notices.
