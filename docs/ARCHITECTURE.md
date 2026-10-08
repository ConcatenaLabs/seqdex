# SeqDEX architecture (2026-07-08 snapshot)

> **Status (2026-08-22): a snapshot of `main` as verified on 2026-07-08,
> superseded where the README says so.** Since then the RFQ cross-chain rail
> was deleted (2026-07-29) and the covenant order book landed on `main`. Read
> with these corrections, which the README and `daemon/README.md` carry in
> full:
>
> - Section 2: `seqdex-xchaind`, `seqdex-xchain-taker` and
>   `seqdex-xchain-reverse-taker` no longer exist, and `/dex/` now serves the
>   SeqDEX website rather than tdexd's REST gateway. Missing from the table:
>   `seqob-settler`, `seqob-watcher`, `seqob-bridge`, `seqob-covenant`,
>   `seqob-relaycli`.
> - Section 3: there is no `seqdex/v1/xchain.proto`.
> - Section 4.1: the `settlement` oneof has a fourth variant, `CovenantTerms
>   = 23` (a funded, self-enforcing resting order), and `LightningTerms` now
>   covers submarine, pure-LN and sub-asset directions.
> - Section 7: tdexd serves same-chain trades only; the `XchainService`
>   bullet describes deleted code.
> - Section 9: pure-Lightning swaps, the continuous matcher and the covenant
>   builder are on `main`. The shipped covenant is a tapscript introspection
>   leaf (`daemon/pkg/covenant`), not Simplicity; the Simplicity design doc
>   describes the design space.
>
> The SeqOB protocol (sections 4.2 to 4.6), the cross-chain safety model
> (5), the anchor-depth dial (6) and the fee model (8) are still as built.

This document describes the system as it existed on `main` on 2026-07-08. It
covers the components and ports, the SeqOB order-book protocol and its
settlement flows (same-chain, cross-chain, submarine), the cross-chain safety
model, the RFQ trade daemon, and the fee model.

## 1. Overview

SeqDEX is non-custodial end to end. Value only ever moves inside an atomic swap
transaction (same-chain) or a pair of hash-time-locked contracts (cross-chain);
no server can spend, freeze, or misdirect user funds. Two subsystems share one
settlement machinery:

```
                         opaque E2E-encrypted courier
   taker (seqob-cli,  <=====[ seqobd relay :9955 ]=====>  maker (seqob-maker)
   web wallet, ...)         signed offers + book               |
        |                                                      | ocean.v1 gRPC
        | on-chain atomic settlement                           v
        +--------------------------------------------->  wallet daemon (oceand)
                                                               |
   trader  --- seqdex.v1 gRPC/REST (:9945, /dex) --->  tdexd (RFQ daemon) ------+
                                                               |                |
                                                        pkg/xchain HTLC engine  |
                                                          (BTC <-> asset)  <----+
```

- **SeqOB** (`daemon/internal/seqob` + the `seqob*` commands): a peer-to-peer
  order book. Orders rest at a relay as signed intents; settlement is
  negotiated directly between the two peers through the relay's blind courier.
- **RFQ daemon** (`daemon/cmd/tdexd`, forked from tdex-daemon): a
  request-for-quote market maker. The trader swaps against the daemon
  operator's own liquidity.
- **`daemon/pkg/xchain`**: the cross-chain HTLC engine both subsystems drive.
- **`wallet/`** (forked from Ocean): the wallet daemon that builds, blinds and
  signs PSETs for the maker sides of both subsystems.

## 2. Components and ports

| Binary | Role | Default listen / target |
|---|---|---|
| `seqobd` | SeqOB relay: book + courier, REST + WS | `:9955` (`SEQOB_LISTEN`) |
| `seqob-maker` | SeqOB maker participant | connects to relay, ocean, nodes |
| `seqob-cli` | SeqOB taker/maker CLI | connects to relay (+ nodes for cross-chain) |
| `seqob-octl` | Ocean account helper for maker setup | connects to ocean |
| `tdexd` | RFQ daemon: Trade gRPC/grpc-web/REST + Operator | Trade `:9945`, Operator `:9000` (`SEQDEX_*` env) |
| `tdex` | Operator CLI for tdexd | connects to `:9000` |
| `oceand` (wallet/) | Wallet daemon (`ocean.v1` gRPC) | `:18000` (`OCEAN_PORT`) |
| `seqdex-xchaind` | Standalone cross-chain maker (bare gRPC), superseded by tdexd's integrated XchainService | `127.0.0.1:9955` (`XCHAIN_LISTEN`; clashes with seqobd's default, rebind one of them if you run both) |
| `seqdex-xchain-taker` / `seqdex-xchain-reverse-taker` | RFQ cross-chain taker CLIs | connect to XchainService |
| `seqdex-xchain-swapdemo` | In-process demo of the raw HTLC mechanism on regtest | - |
| `seqdex-initwallet` / `seqdex-unlock` / `seqdex-market` / `seqdex-taker` | Regtest helpers for the RFQ loop | connect to `:9000` / `:9945` |

On the public testnet the relay is reverse-proxied at
`https://sequentiatestnet.com/seqob/` and the RFQ daemon's REST gateway at
`https://sequentiatestnet.com/dex/`.

## 3. Protocol contracts

The authoritative, current contracts live in `daemon/api-spec/protobuf/`:

- `seqob/v1/offer.proto`, `seqob/v1/relay.proto`: the order-book wire schema
  (offers, cancels, the relay To/From envelope). REST bodies are protojson of
  these messages, so a browser and the Go CLI share one encoding.
- `seqdex/v1/*.proto`: the RFQ contract (Trade, Swap, Transport), forked and
  renamed from `tdex.v2`; PSETv2 only. `seqdex/v1/xchain.proto` adds the
  cross-chain RPCs.
- `ocean/v1/*.proto` (under `wallet/api-spec/`): the wallet contract, kept
  byte-identical to upstream Ocean so the daemon-wallet seam stays drop-in.
- `tdex-daemon/v1,v2`: the inherited operator/wallet-unlocker contracts tdexd
  still serves on `:9000`.

The top-level `proto/` directory is the original standalone copy of the
`seqdex.v1` + `ocean.v1` contracts with local buf codegen (`proto/gen/go`). It
predates the `seqob.v1` and `xchain` additions; treat `daemon/api-spec/` as
current.

## 4. SeqOB: the order book

### 4.1 Offers are signed intents

An `Offer` (see `seqob/v1/offer.proto`, helpers in
`daemon/internal/seqob/offer/`) is a price/size/expiry/keys intent. It is
deliberately **not** a pre-signed transaction and names no UTXOs, so resting
orders cost nothing on-chain, cannot be used to probe a maker's coins, and
cannot go stale against wallet activity. Key properties:

- The authoritative exchange ratio is `want_amount / offer_amount`, both
  integers; there is no floating-point price in the signed bytes. Clients
  derive display prices.
- Canonical bytes are the deterministic protobuf encoding of every field except
  `maker_sig`; the maker signs their SHA256 with secp256k1 (ECDSA/DER) under
  the 33-byte compressed `maker_pubkey`. The signature covers the whole
  `settlement` oneof, so every current and future settlement variant is
  authenticated by construction.
- `expires_at_unix` is mandatory and bounded (relay flags `-min-expiry`,
  `-max-expiry`), so a suppressed cancel self-heals.
- Cancels are separately signed over `{offer_id, maker_pubkey, nonce}`; the
  nonce defeats replaying an old cancel against a re-posted offer id.
- The `settlement` oneof selects the variant: `SameChainTerms` (the maker's
  receive address + blinding pubkey), `CrossChainTerms` (HTLC pubkeys,
  locktime, direction), or `LightningTerms` (submarine-swap terms: direction,
  pubkeys, advisory CLTV, hold-invoice flag, a per-offer 0-conf cap).
- `min_anchor_depth` is the maker's finality dial, see section 6.
- `fee_asset_hint` advertises the maker's preferred fee asset; it is a hint,
  never a privilege (open fee market).

### 4.2 The relay is a blind, non-custodial courier

`seqobd` (`daemon/cmd/seqobd`, API in `daemon/internal/seqob/api/`) does three
things and nothing else:

1. **Stores signed offers** (`internal/seqob/offerstore`) and validates them on
   the way in (`internal/seqob/validator`): signature, schema, amounts,
   direction/asset consistency, expiry bounds, per-pubkey and per-IP rate
   limits, and a replay gate (a byte-identical re-submission of a resting offer
   is acked without consuming rate budget).
2. **Serves the book**: market summaries, per-pair snapshots, and deltas.
3. **Couriers opaque bytes**: when a taker lifts an offer, the session router
   (`internal/seqob/session`) mints a `session_id`, binds the two peers, and
   forwards `SwapMsg{session_id, ciphertext}` frames between them.

The ciphertext is end-to-end encrypted (ECDH between the taker's ephemeral
session key and the maker's offer key, `internal/seqob/client/crypter.go`). The
relay never decrypts, parses, or introspects it, so confidential amounts and
blinding factors never reach the relay. It holds no wallet, no keys, no funds,
and a malicious relay can at worst deny service: each driver's first act on any
courier bytes is an authenticated decrypt, so substituted session keys can
never elicit a signed or funded artifact.

Surface (REST + WebSocket, protojson bodies):

```
POST /v1/offers                             submit a signed Offer
POST /v1/offers/cancel                      submit a signed OfferCancel
GET  /v1/offers?maker_pubkey=...            a maker's own orders
GET  /v1/markets                            market summaries
GET  /v1/market/{base}/{quote}/orderbook    per-pair snapshot (PublicBook)
POST /v1/lift                               open a lift session (StartLift)
GET  /v1/ws                                 WebSocket carrying the To/From envelope
```

Makers hold a WS connection; the relay routes `LiftRequested` notifications to
the online maker of a lifted offer and evicts a maker's offers on disconnect.

### 4.3 Order lifecycle

Statuses (`seqob/v1/relay.proto`): `OPEN -> PARTIAL -> FILLED`, or `CANCELLED`,
`EXPIRED`, `UTXO_INVALIDATED` (the maker could not co-sign because its coins
moved; the order is dropped at zero on-chain cost).

`FILLED` is anchor-aware: a fill is applied with its `settle_txid` and current
anchor confirmation count, and the order only reaches `FILLED` when the
settling transaction has at least `min_anchor_depth` Bitcoin-anchor
confirmations (`offerstore.ApplyPartialFill`). With the default
`min_anchor_depth=0` that is immediate, and honestly so: `OrderStatus` exposes
`anchor_confs` so clients can display real depth, and the store has a `Reopen`
hook for returning an order to the book if its settling transaction's Bitcoin
anchor is later orphaned (the swap un-happens, so the order must come back).

**Stub disclosure:** on `main` the relay's reorg watcher and maker-liveness
probe are explicit no-op stubs (`session.NoopReorgWatcher`,
`validator.NoopLivenessProbe`). Anchor-orphan re-opening is designed and
plumbed but does not fire yet; the parties' own refund/timeout paths are the
safety net, and the relay never holds anything in any case.

### 4.4 Same-chain lift (asset for asset, one atomic transaction)

Drivers in `daemon/internal/seqob/client/` (`driver.go`, wallet seam in
`wallet.go` / `live.go`):

1. Taker: `POST /v1/lift` with an ephemeral session pubkey; relay opens the
   session (default co-sign deadline 2 minutes) and notifies the maker.
2. Taker builds a `SwapRequest` (a PSETv2 with its inputs and outputs, plus
   unblinded input data for the maker's verification), seals it, couriers it.
3. Maker opens it and settles through the proven Ocean path
   (`wallet.Service.CompleteSwap`): it verifies the proposal against the SIGNED
   offer's terms (never against relay-supplied data), adds its inputs and
   outputs and the network-fee block (or validates the taker-supplied fee
   output), signs its half, and couriers back a `SwapAccept`.
4. Taker verifies the completed PSET still matches the lift terms, blinds
   (when confidential), signs, broadcasts, and couriers a `SwapComplete` with
   the txid.

Confidentiality is opt-in per offer, matching Sequentia's transparent-by-default
design: a confidential offer publishes a blinding pubkey and settles blinded;
an explicit offer settles unblinded.

### 4.5 Cross-chain lift (BTC on-chain, `xcourier.go` / `xdriver*.go`)

Cross-chain offers pair an asset with the literal sentinel `"BTC"`
(`internal/seqob/offer/sentinel.go`). The handshake messages are JSON sealed
inside the same opaque courier; settlement is `pkg/xchain` (section 5). Relay
sessions on cross-chain offers get a longer deadline (default 3 hours,
`seqobd -xsession-deadline`) because a real parent-chain confirmation happens
between courier rounds.

FORWARD (`direction = BTC_TO_ASSET`, taker pays BTC and holds the secret):

```
taker -> XcTermsRequest
maker -> XcTerms          (pubkeys, locktimes, amounts, fee)
taker    locks the BTC HTLC, waits out min-btc-conf confirmations
taker -> XcBtcLegFunded   (hash H, BTC leg, taker pubkeys)
maker    verifies the BTC leg byte-for-byte, locks the Sequentia leg
maker -> XcSeqLegLocked   (Sequentia leg + block hash + anchor height)
taker    VerifySeqLegSafe (anchor gate), then claims the asset,
         revealing the secret on Sequentia
maker    watches the claim, extracts the secret, claims the BTC
```

REVERSE (`direction = ASSET_TO_BTC`, taker sells the asset; the MAKER holds the
secret and funds BTC first):

```
taker -> XcTermsRequest   (taker's BTC claim + Sequentia refund pubkeys)
maker -> XcBtcLegLocked   (hash H, funded BTC leg, terms)
taker    verifies the BTC leg, awaits its confirmation, funds the Sequentia leg
taker -> XcSeqLegFunded   (Sequentia leg + block hash + anchor height)
maker    VerifySeqLegSafe, claims the asset (revealing the secret)
maker -> XcSecretRevealed (courtesy; the secret is also on-chain)
taker    claims the BTC
```

Abort at any point falls back to the CLTV refunds: `seqob-cli xrefund` (BTC
leg) and `xrefund-seq` (asset leg). The maker persists per-lift session state
(`seqob-maker -xstate-dir`) and `seqob-maker -resume` finishes every
non-terminal session (claim or refund) after a crash or restart.

### 4.6 Submarine lift (BTC over Lightning, `xcourier_submarine.go`)

Offers with `LightningTerms` swap an on-chain Sequentia asset against BTC paid
over vanilla Lightning; the maker runs a SeqLN/CLN node on Bitcoin
(`seqob-maker -mode lightning -ln-socket ...`) and plays the non-custodial
Boltz-style role. The BTC leg is a BOLT11 payment; the asset leg and the anchor
gate are the same `pkg/xchain` primitives.

NORMAL (`ln_direction = ASSET_ONCHAIN_FOR_BTC_LN`, taker sells the asset,
receives BTC over Lightning; secret holder = taker; `seqob-cli xsublift`):
the taker mints a BOLT11 invoice on its preimage, funds the asset HTLC, and
sends both; the maker waits until the funding is anchor-buried at least
`min_anchor_depth` (its `-sub-anchor-depth`, default 3, floor 2), pays the
invoice (learning the preimage), and claims the asset. If the maker never pays,
the taker refunds after the CLTV.

REVERSE (`ln_direction = BTC_LN_FOR_ASSET_ONCHAIN`, taker buys the asset with
Lightning BTC; `seqob-cli xsubbuy`): the maker locks the asset HTLC and sends
its invoice; the taker anchor-gates the maker's HTLC **before** paying, then
pays, learns the preimage, and claims the asset on-chain.

The submarine rule of thumb: whoever is about to take an **irreversible
Lightning action** first requires real anchor depth on the Sequentia side,
because a Lightning payment cannot be reorganized away but a 0-conf Sequentia
transaction can.

## 5. Cross-chain settlement safety (`daemon/pkg/xchain`)

The engine implements a classic single-secret HTLC pair (hashlock + CLTV
refund) with two Sequentia-specific properties:

1. **Anchor-shortened ordering instead of reorg buffers.** A Sequentia block
   reverts if and only if its anchored Bitcoin block reverts (Bitcoin anchoring
   is supreme consensus law; the node's
   `feature_anchor_swap_consistency.py` proves the swap-leg consequence). So
   the BTC leg is locked first, and the Sequentia leg is accepted once
   `VerifySeqLegSafe` passes: the leg's block has `anchorheight >=` the BTC
   leg's confirmation height, `getanchorstatus` is ok, and the block is
   quorum-certified. No day-long timelocks, no N-confirmation buffer on the
   Sequentia side; this is what makes cross-chain swaps against a ~10-minute
   parent chain practical.
2. **Reorg-aware finality everywhere.** A "settled" Sequentia leg is only as
   final as its anchor; nothing in the codebase treats a Sequentia transaction
   as irreversibly final at 0 anchor confirmations (this deliberately corrects
   the upstream TDEX assumption of Liquid-style immediate finality).

The BTC side is protected the ordinary Bitcoin way: confirmation depth
(`-min-btc-conf`, default 1, raise for real value). Timelocks are asymmetric,
`T_btc > T_seq` in wall-clock terms, so the secret holder can never profit from
stalling. The hashlock is abstracted (`primitive.go`) so an adaptor-signature
(PTLC) variant can replace SHA256 without re-architecting.

`chain.go` speaks to an Elements-format parent; `chain_bitcoin.go` /
`leg_bitcoin.go` / `btc_backend.go` implement the same leg on a real `bitcoind`
(regtest or testnet4). `submarine.go` swaps the on-chain BTC leg for an `LNLeg`
(`leg_lightning.go`, a CLN JSON socket client). On a Sequentia node a Lightning
leg is in one asset: its holds and invoices name it, a hold accepted in another
asset is refused, and it pays only an invoice whose `a` field names that asset,
refusing any other before an HTLC exists.

## 6. The `min_anchor_depth` dial (0-conf honesty)

Nothing is "final" at 0 confirmations; anchor depth is the honest finality
measure. Policy, implemented exactly:

- `min_anchor_depth` is **per-offer and maker-chosen, default 0**. The
  validator enforces no floor; the DEX is 0-conf-tolerant.
- The relay marks an order `FILLED` only once the settling transaction has that
  many Bitcoin-anchor confirmations, and exposes the running `anchor_confs` so
  clients can render depth honestly instead of a fake "final".
- Cross-leg points that are irreversible (paying a Lightning invoice, settling
  a hold invoice) require a real depth (>= 2, default 3): these are the places
  where 0-conf tolerance would let a Bitcoin reorg turn atomicity into theft.
- Exchanges or merchants wanting stronger guarantees simply wait more anchor
  depth; roughly 3 anchor confirmations is a reasonable point-of-sale bar.

## 7. The RFQ trade daemon (`cmd/tdexd`)

The forked tdex-daemon serves three interfaces:

- **Trade** (`:9945`, multiplexed gRPC + grpc-web + grpc-gateway REST with
  permissive CORS; TLS optional): `ListMarkets`, `GetMarketPrice`,
  `PreviewTrade`, `ProposeTrade`, `CompleteTrade` under REST paths
  `/v1/markets`, `/v1/market/price`, `/v1/trade/{preview,propose,complete}`.
  A trade is the same cooperative atomic swap: the trader proposes a PSET, the
  daemon's wallet completes and co-signs it, the trader finalizes.
- **Operator** (`:9000`, gRPC, macaroon-authenticated by default): market
  lifecycle, deposits/withdrawals, fee config; driven by the `tdex` CLI.
- **XchainService** (folded onto the Trade listener when `SEQDEX_XCHAIN_*` is
  configured): quote/propose/status RPCs for cross-chain swaps in both
  directions under `/v1/xchain/*` (`markets`, `quote`, `propose`, `swap`,
  `reverse/quote`, `reverse/open`, `reverse/submit`). The taker-side gate is
  the same `VerifySeqLegSafe` logic; reference takers are
  `cmd/seqdex-xchain-taker` and `cmd/seqdex-xchain-reverse-taker`.

The daemon is wallet-detached: everything runs against the
`internal/core/ports` wallet interface, implemented over `ocean.v1` gRPC by
`internal/infrastructure/ocean-wallet`, pointed at the Sequentia-adapted
`wallet/` daemon. That seam is kept byte-identical to upstream.

## 8. Any-asset fees (open fee market)

Sequentia has no privileged fee asset. The daemon reads the node's fee
whitelist and exchange rates via `getfeeexchangerates` (env `SEQDEX_NODE_RPC`;
`internal/core/application/wallet/feerates.go`) and applies them in
`CompleteSwap` (`internal/core/application/wallet/service.go`):

- The network fee of a swap defaults to **the transacted asset** (the asset the
  proposer receives), converted from the size-based native-equivalent fee at
  the node's rate, rounded up. If that asset is not fee-eligible the daemon
  falls back to the policy asset (tSEQ) from the fee account.
- A **taker-funded fee** is supported: the proposer may include one explicit
  fee output in any fee-eligible asset; the maker validates that its
  native-equivalent value covers the required fee and every taker input is
  revealed, then skips its own fee block. Wallet-side fee handling for plain
  (non-swap) transfers pays in the transacted asset the same way.
- SeqOB carries the same model through `StartLift.taker_fee_asset` and the
  offer's `fee_asset_hint`.

A valuable asset paying few atoms of fee is correct (the fee's value, not its
atom count, is what the rate preserves). Fee rates are denominated in the
chosen fee asset's own units per vByte.

## 9. History and upstream

SeqDEX began as a fork of the TDEX stack (`tdex-daemon` + `tdex-protobuf`) and
the Ocean wallet daemon, both MIT (see `NOTICE`). The RFQ daemon and wallet
keep their upstream shape; the Sequentia-specific work is the `seqnet` network
parameters, the runtime-derived policy asset (never a compile-time constant),
the JSON-RPC block scanner that tolerates Sequentia's anchored PoS headers, the
any-asset fee path, reorg-aware finality, and everything under
`internal/seqob` and `pkg/xchain`, which is new code. The order book was built
here; TDEX has no order book.

Work not on `main`: pure-Lightning swaps (both legs off-chain) and the
continuous CLOB matcher with covenant-enforced (Simplicity) passive resting
orders live on the `phase3-pure-ln` branch. Documentation for those belongs to
that branch; nothing on `main` consumes Simplicity.
