# SeqDEX

SeqDEX: non-custodial atomic-swap DEX for [Sequentia](https://sequentia.io), a
Bitcoin sidechain for asset tokenization and disintermediated exchanges. It provides
a peer-to-peer order book (SeqOB), same-chain asset-to-asset atomic swaps, and
cross-chain BTC-to-asset swaps whose safety comes from Sequentia's real-time
Bitcoin anchoring. No component ever takes custody of user funds: settlement is
always an atomic transaction (same-chain), a hash-time-locked contract pair
(cross-chain) or a consensus-enforced covenant spend (passive resting orders),
and the order-book relay holds no keys, no wallet, and no funds.

Everything here is **testnet software**. There is no Sequentia mainnet.

## Where this fits in the Sequentia ecosystem

| Repo | One-liner |
|---|---|
| [`Sequentia`](https://github.com/ConcatenaLabs/Sequentia) | The Sequentia node (Sequentia Core, a fork of Elements 23.3.3; binaries `sequentiad` / `sequentia-cli`): consensus, anchoring, proof of stake, open fee market, plus the canonical protocol documentation in `doc/sequentia/`. |
| [`seqdex`](https://github.com/ConcatenaLabs/seqdex) | SeqDEX: non-custodial atomic-swap DEX - P2P order book (seqob), same-chain swaps, and cross-chain BTC<->asset swaps made safe by Bitcoin anchoring. |
| [`seqdex-web`](https://github.com/ConcatenaLabs/seqdex-web) | The SeqDEX website, live at https://sequentiatestnet.com/dex/: three trading surfaces over the relay mounts, settling through the Ambra browser extension. |
| [`SWK`](https://github.com/ConcatenaLabs/SWK) | Sequentia Wallet Kit: a fork of Blockstream LWK - Rust wallet library, CLI, and WASM bindings for building Sequentia (and Bitcoin testnet4) wallets. |
| [`sequentia-web-wallet`](https://github.com/ConcatenaLabs/sequentia-web-wallet) | Proof-of-concept browser wallet built on SWK, live at https://sequentiatestnet.com/wallet. |
| [`seqln`](https://github.com/ConcatenaLabs/seqln) | SeqLN: a Core Lightning fork that runs on Sequentia and Bitcoin from the same binary - asset channels, any-asset payments, pure-Lightning swaps. |

Protocol-level background (anchoring, proof of stake, the open fee market) lives
in the node repo's documentation:
https://github.com/ConcatenaLabs/Sequentia/tree/master/doc/sequentia

## What is in this repo

Two cooperating DEX subsystems plus the shared settlement engines, all in Go:

1. **SeqOB, the peer-to-peer order-book DEX** (`daemon/internal/seqob`,
   `daemon/pkg/covenant`, and the `daemon/cmd/seqob*` commands). Interactive
   resting orders are **signed intents** (price, size, expiry, keys), never
   pre-signed transactions and never named UTXOs. A non-custodial relay
   (`seqobd`) stores the signed offers, serves the per-pair book over REST and
   WebSocket, matches crossing orders, and couriers **opaque, end-to-end-
   encrypted** swap-session messages between maker and taker: it never
   decrypts, never signs, and never touches funds. Settlement happens directly
   between the two peers. **Covenant resting orders** are the passive
   complement: the maker funds one taproot UTXO whose tapscript leaves enforce
   the price, so anyone can fill it while the maker is offline, and two such
   orders are settled against each other by the keyless `seqob-settler`. The
   chain-watcher keeps the covenant book consistent with the chain, and
   `seqob-bridge` settles a covenant order against a Lightning order.

2. **The RFQ trade daemon** (`daemon/cmd/tdexd` plus the `wallet/` Ocean fork),
   a liquidity-provider daemon forked from the TDEX stack. A trader asks it for
   a quote and settles a cooperative same-chain atomic swap against the daemon's
   own market-maker wallet, over gRPC, grpc-web and REST. Cross-chain
   settlement is order-book only (item 3); the RFQ daemon does not do it.

3. **The cross-chain HTLC engine** (`daemon/pkg/xchain`), driven by the SeqOB
   maker, CLI, settler and bridge: BTC-to-asset atomic swaps between Bitcoin
   (testnet4 or a regtest stand-in) and Sequentia, in both directions, with the
   BTC leg on-chain (HTLC), over Lightning (submarine swaps via a SeqLN/CLN
   node), both legs over Lightning (pure-LN), or the asset over Lightning
   against on-chain BTC (sub-asset rails).

Both subsystems settle same-chain swaps through the same proven path: the
`wallet/` daemon (a thin fork of Ocean) builds, blinds when requested, and
co-signs the atomic swap PSET.

## What settles, and how it is proven

Each of these is exercised on the public testnet or in the committed test
suites:

- SeqOB relay + maker + taker CLI: same-chain asset-to-asset lifts, partial
  fills, signed cancels, offer expiry, replay protection, rate limits. The
  chain-watcher follows each lift's settling transaction, as its maker
  acknowledges it, under the same finality rule as a covenant fill, and
  serves its record beside them (`GET /v1/market/{base}/{quote}/fills`, taker
  `interactive`): pending, confirmed, final once certified and anchor-buried,
  undone with its trade when its block leaves the chain, and recorded again if
  it is mined again.
- Covenant-funded passive resting orders (tapscript introspection covenants):
  permissionless fills, partial fills with a re-rested remainder, joint
  settlement of two passive orders by `seqob-settler`, the continuous matcher
  in the relay, the chain-watcher (a record of each fill read from the filling
  transaction's outputs, final once certified and anchor-buried, undone with
  its trade when its block leaves the chain; ghost removal, remainder
  re-resting), and the rail-crossing bridge. Proven at consensus level by
  `test/regtest/feature_seqob_*.py` against a real node.
- Cross-chain order-book settlement over the relay courier, both directions:
  forward (taker pays BTC, receives the asset) and reverse (taker sells the
  asset for BTC), with CLTV refund paths and maker crash-resume from persisted
  session state.
- Submarine swaps (asset on-chain against BTC over Lightning, both directions),
  pure-Lightning swaps (`-mode pureln`, both legs over SeqLN channels, no
  on-chain leg and no anchor wait) and the sub-asset rails (`-mode subasset` /
  `subasset-sell`: asset over Lightning against an on-chain BTC HTLC, either
  direction). All need a running SeqLN node per party.
- RFQ same-chain trades (preview, propose, complete) over gRPC/REST.
- Network fees payable in any fee-eligible asset, defaulting to the transacted
  asset (see "Fee model" below).

Deliberate gaps, stated honestly:

- The relay's maker-liveness probe and its settlement reorg watcher for
  **interactive** orders are no-op stubs. If an interactive settlement's
  Bitcoin anchor were orphaned, the relay does not re-open the order
  automatically (the on-chain refund paths, not the relay, are the safety net;
  the relay never holds anything either way). Covenant orders are covered by
  the chain-watcher.
- Order and session state in the relay and the SeqOB maker's same-chain path is
  in-memory (`seqobd -trade-log` persists the trade feed); cross-chain sessions
  are persisted (`-xstate-dir`) and resumable, same-chain lifts simply expire
  and the book re-forms.

## The live public testnet deployment

The public Sequentia testnet exposes the order book under
https://sequentiatestnet.com:

- **SeqOB relays**, one per rail: `https://sequentiatestnet.com/seqob/v1/...`
  (on-chain: same-chain, covenant and cross-chain offers), `/seqob-pln/v1/...`
  (Lightning rails) and `/seqob-conf/v1/...` (confidential markets). For
  example `GET /seqob/v1/markets` lists the markets; a cross-chain pair's quote
  asset is the literal sentinel `"BTC"`.
- `https://sequentiatestnet.com/dex/` is the SeqDEX website (`seqdex-web`),
  not an API. The RFQ daemon's REST gateway is not publicly exposed.

Try it:

```sh
curl -s https://sequentiatestnet.com/seqob/v1/markets | python3 -m json.tool | head -30
```

The web wallet at https://sequentiatestnet.com/wallet and the SeqDEX site at
https://sequentiatestnet.com/dex/ are the graphical clients: their trade
surfaces consume these relays. The CLIs in this repo (`seqob-cli` with its
`xlift`/`xsell`/`xsublift`/`xsubbuy`/`xpln`/`xsubas` subcommands, and
`seqdex-taker` for the RFQ loop) are the reference command-line clients.

## How a trade works

**Same-chain, over the order book.** A maker posts a signed offer (for example,
sell 100 GOLD atoms for 4500 USDX atoms; the authoritative price is the integer
ratio `want_amount / offer_amount`, there is no float in the signed bytes). A
taker picks the offer from the book and opens a lift session. Relay-couriered,
end-to-end-encrypted messages carry a `SwapRequest` from the taker, a
`SwapAccept` (the maker's half-signed PSET) back, and a `SwapComplete`; the
taker signs and broadcasts one atomic transaction that pays both sides. At no
point can either party or the relay take the other's funds: an unsigned or
half-signed swap is worthless, and the worst outcome is a session timeout.

**Covenant resting orders (passive).** The maker locks the offered asset in one
taproot UTXO with a NUMS internal key and a `{FILL, REFUND}` leaf tree; the FILL
leaf bakes in the asset pair, price, payout program and minimum lot, enforced by
the node's tapscript introspection opcodes. The relay only advertises the
outpoint (`CovenantTerms`); a taker re-derives the output key and verifies it
against the chain, then spends the UTXO in a fill that pays the maker its pinned
price. The order *is* the coin, so oversell is impossible and the maker can be
offline. The REFUND leaf returns the coin to its maker once the order's
`expiry_locktime` has passed: a block height below 500,000,000, a median time
past (unix seconds) from it. New orders from the web wallet take a median time
a day ahead, since a height lock opens later than planned after an anchor
rollback disconnects blocks, and an operator that fills only orders whose
refund is hours away would have to turn heights into hours at an assumed block
rate. The maker's credit program (`maker_prog`) must be a plain key, never an
Arca leaf or board script: one maker-and-operator signature pair over such a
script spends two equal credits paid to it at once. The leaf binds the order spent at input `k` to its maker's credit at
output `2k` and its remainder at `2k+1`, so one transaction can fill several
orders, at inputs `0..n-1`, with everything else after their `2n` outputs: a
taker taking several books at once (`seqob-cli covfill` with several offers),
or an operator's round filling the orders its takers asked for. Two passive
orders that cross are settled in one transaction by `seqob-settler`, which
funds the fee but cannot alter a payout. The design
space is in [docs/simplicity-dex-covenant-offers-design.md](docs/simplicity-dex-covenant-offers-design.md);
the order book's leaf is tapscript (`daemon/pkg/covenant`).

`daemon/pkg/covenant` also builds the Simplicity order of the template
`seqdex/order` (sequentia-contracts). Version 2 (`templates/seqdex_order_2`,
copied in `daemon/pkg/covenant/seqdex_order_2/`) is what every new order uses.
Version 1 (`templates/seqdex_order`, copied in `daemon/pkg/covenant/seqdex_order/`)
is the same program without one rule, that a spend carries no annex: a fill
signs nothing, so whoever relays a version 1 fill can attach an annex and bring
its fee rate down to the relay floor. Version 1 orders already resting are still
derived, listed and filled. Each copy is pinned to a commit by its `PIN.json`,
and an offer names its version by the template hash it signs.
It is one program for every order, the terms held as data in a leaf beside it,
so an order's address is the pinned commitment root plus its terms and Go
derives it with no compiler. Its fill compares the price on 128-bit products,
so any 64-bit amount and price fills, where the tapscript leaf must keep
`filled × rate_num` under 2^63 and reads any term of 2^63 or more as negative
(the Go builder, the validator and the web wallet refuse such terms). Every spend's pruned program is a constant of
the template, so Go builds each fill's witness with no compiler too; a refund
needs the refund key's signature over the transaction's Simplicity
`sig_all_hash`. A fill in the same layout costs more: 921 vB against 668 vB for
a partial fill, 814 vB against 590 vB for a whole one (version 1: 917 and 811).

Such an order rests in the book under its own settlement member,
`SimplicityCovenantTerms` (`offer.proto`), its outpoint outside the signed
bytes as for `CovenantTerms`. The relay validates it (a version this build
pins, the price and assets the offer advertises, an expiry), probes its
outpoint on the chain, and its watcher records its fills and re-points it at
`(txid, 2k+1)` after a partial one. `seqob-cli covfill` takes it, alone or
beside tapscript orders in one transaction, and so can an operator's round.
The matcher does not auto-cross it, because the owners it hands a match to
(the settler, the bridge, the `Matched` message) carry tapscript terms only,
and the crosser skips it for the same reason: it is filled by a taker who
reads it from the book.

An order can also rest as a leaf in an Arca operator's tree (the `order-3`
template, or `order-2` for one resting from before it), under the member `RestingLeafTerms` (`offer.proto`). Its maker
rests it and leaves; takers fill it through the operator, each fill co-signed
by the operator and held to the order's terms by the leaf's own program, and
a part fill rests the remainder again at a new coin. The offer signs the
terms, the operator's URL and key and the order id (the leaf id of the
order's first coin); its locator, the coin it rests at now and what that coin
holds, is outside the signed bytes, as an on-chain order's outpoint is.

**Cross-chain (BTC on-chain).** The legs are two HTLCs sharing one SHA256
hashlock, with asymmetric timelocks (the BTC leg always refunds later than the
Sequentia leg). Forward direction: the taker locks BTC first, the maker locks
the Sequentia asset, the taker claims the asset (revealing the secret on
Sequentia), the maker claims the BTC. What makes this fast and safe is
Sequentia's anchoring: the claimant accepts the Sequentia leg only once it sits
in a block whose Bitcoin anchor height is at or above the BTC leg's
confirmation height, with `getanchorstatus` ok and the block quorum-certified.
Because a Sequentia block reverts if and only if its anchored Bitcoin block
reverts, no extra reorg buffer is needed on the Sequentia side. If anything
stalls, each party refunds its own leg after its timelock. See
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the exact message flows.

**Submarine (BTC over Lightning).** Same hashlock idea, but the BTC leg is a
BOLT11 Lightning payment instead of an on-chain HTLC. The maker plays the
non-custodial Boltz-style role and must not take the irreversible Lightning
action until the relevant Sequentia transaction is buried under a real Bitcoin
anchor depth (default 3, minimum 2).

**Pure Lightning and sub-asset.** With both legs over Lightning (the asset on a
SeqLN asset channel, BTC on a SeqLN-on-Bitcoin channel) a trade is a pair of
hold invoices under one hash: nothing touches a chain, so there is no anchor
wait. The sub-asset rails are the submarine's mirror: the asset travels over
Lightning while BTC is an on-chain HTLC.

## Finality honesty

Nothing on Sequentia is "final" at 0 confirmations. The honest finality measure
is **anchor depth**: how many Bitcoin blocks confirm the anchor of the
Sequentia block containing your settlement.

Every SeqOB offer carries a maker-chosen dial, `min_anchor_depth` (default 0):
the number of Bitcoin-anchor confirmations the maker requires on the settling
transaction before the order is marked `FILLED`. The DEX is 0-conf-tolerant by
design and enforces no floor; a maker who wants stronger guarantees (or a taker
choosing whom to trade with) raises the dial. Client UIs surface anchor depth
rather than pretending instant finality, and a status of `FILLED` at depth 0
can still revert if Bitcoin reorganizes: that is a property of every chain, and
Sequentia's design makes it visible instead of hiding it.

## Fee model

Sequentia has an open fee market: there is no privileged fee asset, and block
producers choose which assets they accept for fees and at what exchange rate
(published by the node as `getfeeexchangerates`). SeqDEX implements this
end-to-end:

- The on-chain network fee of a swap is paid **in any fee-eligible asset,
  defaulting to the transacted asset**, valued at the node's native-equivalent
  exchange rate. The legacy native-asset fee account is only a fallback.
- A taker can fund the network fee itself by adding an explicit fee output in
  its chosen asset; the maker's wallet validates it (correct value at the
  current rate) and then skips its own fee block.
- SeqOB offers carry a `fee_asset_hint`; it is a hint, never a privilege.
- Fee rates are always denominated in the chosen fee asset's own units per
  vByte, never "sat/vB".

## Running it yourself

Prerequisites: a recent Go toolchain (the daemon's release build uses CGO),
and for full local loops a built Sequentia
node (`sequentiad` and `sequentia-cli` from
https://github.com/ConcatenaLabs/Sequentia).

Build every binary:

```sh
cd daemon && go build ./... && cd ../wallet && go build ./...
# or specific binaries, e.g.:
cd daemon
go build -o ../bin/seqobd        ./cmd/seqobd
go build -o ../bin/seqob-maker   ./cmd/seqob-maker
go build -o ../bin/seqob-cli     ./cmd/seqob-cli
go build -o ../bin/seqob-settler ./cmd/seqob-settler
go build -o ../bin/tdexd         ./cmd/tdexd
cd ../wallet
go build -o ../bin/seqdex-wallet ./cmd/oceand
```

Run the unit tests (no node required; the cross-chain integration test
self-skips unless it finds a node, see [docs/DEV.md](docs/DEV.md)):

```sh
cd daemon && go test ./internal/seqob/... ./pkg/xchain/... ./pkg/covenant/...
```

[docs/DEV.md](docs/DEV.md) walks through the full local loop: a two-chain
regtest (parent chain + anchored Sequentia), the wallet daemon, the relay, a
maker, and a taker lift, plus the cross-chain, Lightning and covenant variants.

## Repo layout

```
daemon/                The Go daemon module (fork of tdex-daemon).
  cmd/seqobd/          SeqOB relay: non-custodial order book, matcher, opaque
                       courier; runs the covenant chain-watcher when given
                       -node-rpc.
  cmd/seqob-maker/     SeqOB maker: posts offers, settles lifts. Modes:
                       samechain, cross, lightning, pureln, subasset,
                       subasset-sell. Anyone can run one.
  cmd/seqob-cli/       SeqOB taker CLI: post/book/lift plus the cross-chain,
                       submarine, pure-LN and sub-asset taker commands.
  cmd/seqob-settler/   Always-online, keyless settler for two crossing covenant
                       orders (plan | run).
  cmd/seqob-watcher/   The covenant chain-watcher as a standalone binary
                       (classify | run); seqobd embeds the same core.
  cmd/seqob-bridge/    Cross-rail settlement bridge: fills a covenant order
                       against a Lightning order under one preimage. The only
                       seqob component holding its own inventory and LN node.
  cmd/seqob-covenant/  Covenant builder CLI: scriptPubKey and FILL witness
                       for a tapscript order, byte-identical to the proven
                       Python (derive | fill | filltx | check); address,
                       witness and judgement of a Simplicity order (simderive
                       | simwitness | simcheck); a maker's pool and a
                       clearing round at one price (poolderive | roundderive
                       | round | roundsign | poolfill | poolwithdraw |
                       roundrefund).
  cmd/seqob-crosser/   Crossing agent: polls every relay, builds the union
                       book per pair, and when a resting bid crosses a resting
                       ask it takes BOTH sides with its own inventory. One
                       participant, not infrastructure; anyone can run one.
  cmd/seqob-relaycli/  Scripting client for the relay (post | take), used by
                       the regtest matcher proof.
  cmd/seqob-octl/      Helper for inspecting/preparing Ocean maker accounts.
  cmd/tdexd/           RFQ trade daemon (Trade :9945, Operator :9000).
  cmd/tdex/            Upstream operator CLI for tdexd.
  cmd/seqdex-*         tdexd regtest helpers + the raw-HTLC swap demo.
  internal/seqob/      Order book: offer signing, store, validator, matcher,
                       session router, E2E courier client, lift drivers,
                       covenant fill/settler/watcher/bridge cores, reorg.
  pkg/covenant/        The covenant leaf builders: the tapscript FILL/REFUND
                       leaf (golden-vector pinned against the Python in
                       test/regtest) and the Simplicity order (pinned to the
                       template's vectors in seqdex_order_2/ and
                       seqdex_order/).
  pkg/xchain/          Cross-chain HTLC engine + submarine, pure-LN and
                       sub-asset swaps + regtest harness
                       (testdata/start-regtest.sh).
  api-spec/protobuf/   The authoritative protocol contracts (seqdex.v1,
                       seqob.v1, ocean.v1, legacy tdex-daemon operator protos).
wallet/                Sequentia wallet daemon (thin fork of Ocean); tdexd and
                       seqob-maker settle through it. Builds `oceand`.
proto/                 Standalone phase-1 copy of the seqdex.v1 + ocean.v1
                       contracts with local buf codegen (predates seqob.v1;
                       daemon/api-spec is current).
docs/                  Design and handover notes, see the list below.
test/regtest/          Consensus-level proofs of the covenant order book, run
                       against a real Sequentia node (see its README).
```

The `docs/` directory. These describe how the system works now:

- `DEV.md` - local runs of every loop.
- `seqdex-terminal-spec.md` - the product spec for the wallets' Swap tab
  (cross-repo, self-declared canonical).
- `simplicity-dex-covenant-offers-design.md` (+ `.html`/`.pdf`) - the design of
  the covenant rail; the shipped leaf is tapscript introspection.
- `rail-crossing-p2p-lsp-design.md` - the rail-crossing design, partially
  implemented (`cmd/seqob-bridge`).
- `ARCHITECTURE.md` - the message flows, the cross-chain safety model and the
  fee model. Its header names the parts it has outlived; read that first.

The remaining files in `docs/` are design and handover notes kept as history.
They record how a decision was reached, not how the code behaves, so take
behaviour from the documents above or from the code itself.

Component docs: [daemon/README.md](daemon/README.md),
[wallet/README.md](wallet/README.md).

## Contributing

`main` is the integration branch; open pull requests against it.

## License

MIT, see [LICENSE](LICENSE). SeqDEX is a fork of the
[TDEX](https://github.com/tdex-network) stack (`tdex-daemon`, `tdex-protobuf`)
and the [Ocean](https://github.com/vulpemventures/ocean) wallet daemon; the
upstream MIT notices are retained in [NOTICE](NOTICE).
