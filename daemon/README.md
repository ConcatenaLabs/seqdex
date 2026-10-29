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
  -trade-reporters   comma-separated compressed public keys (hex) of the
                     makers whose own trade reports are recorded: in-tree
                     swaps and BTC desk trades the relay cannot watch
                     (env SEQOB_TRADE_REPORTERS; empty = none)
  -trade-follow      comma-separated base URLs of other relays whose trades
                     this relay's market view includes, so one relay serves
                     every rail's trades (env SEQOB_TRADE_FOLLOW)
  -covenant-book     file keeping the covenant orders, both sides of each
                     maker's pool, and the orders resting in operators'
                     trees, across a restart; each is checked again (against
                     the chain, or its operator) when read back (env
                     SEQOB_COVENANT_BOOK; empty = in memory only)
  -interactive-cap   soft per-maker committed-capital cap per offered asset
                     (atoms) for interactive offers; past it an offer is
                     flagged demoted (phantom depth); 0 disables
  -fill-anchor-depth Bitcoin blocks that must bury a covenant fill's anchor,
                     its block certified, before the relay records the fill
                     final (default 2; an offer's min_anchor_depth raises it)
  -arca-operator     comma-separated base URLs of Arca operators the relay
                     follows: a covenant fill made by one of their rounds is
                     recorded as that round's, and orders resting in their
                     trees are listed and followed (env SEQOB_ARCA_OPERATORS;
                     needs -node-rpc)
  -resting-interval  how often the relay asks each operator how the orders
                     resting in its tree stand (default 3s)
```

HTTP surface (bodies are protojson of `api-spec/protobuf/seqob/v1`):

```
POST /v1/offers                             submit a signed Offer
POST /v1/offers/cancel                      submit a signed OfferCancel
GET  /v1/offers?maker_pubkey=<hex>          a maker's own orders
GET  /v1/markets                            market summaries
GET  /v1/market/{base}/{quote}/orderbook    per-pair snapshot: the signed
                                            offers, and statuses[i], the live
                                            state of offers[i]
GET  /v1/market/{base}/{quote}/trades       recent trades, every rail's (JSON)
GET  /v1/market/{base}/{quote}/candles      OHLCV from the trades (JSON)
GET  /v1/market/{base}/{quote}/ticker       last price, the last 24 hours'
                                            open/high/low/change/volume, best
                                            bid and ask (JSON)
GET  /v1/tickers                             every market's ticker in one
                                            answer, each in the market's
                                            canonical direction, with spark_7d
                                            (JSON)
GET  /v1/market/{base}/{quote}/fills        fill records of covenant and resting
                                            orders (JSON)
GET  /v1/rounds                             clearing rounds: each round's one
                                            price, its round orders' fills, the
                                            pools it moved, and its state (JSON)
GET  /v1/pools/moves[?pool=KEY]             each pool's fills and withdrawal
                                            (JSON)
GET  /v1/trades                             every market's trades this relay
                                            recorded itself, for a relay that
                                            follows it (JSON)
POST /v1/trades/report                      a trusted maker's signed record of
                                            a trade of its own (JSON)
POST /v1/lift                               open a lift session
GET  /v1/ws                                 WebSocket (To/From envelope): book
                                            subscriptions, offer submission,
                                            lift and match notifications,
                                            opaque SwapMsg courier
```

### Market data

A market is the unordered pair of two assets. An order selling A for B and one
selling B for A trade in the same market, so the relay keeps one trade log per
market, in its canonical direction (the asset whose id sorts first is the base),
and serves `trades`, `candles` and `ticker` in whichever direction the path
names: `/v1/market/A/B/...` and `/v1/market/B/A/...` show the same trades, one the
inverse of the other. Each trade in `trades` carries `price` (quote atoms per base
atom), `size` (base atoms), `quote_size` (quote atoms), `side` (`buy` when the
taker received the base), its `rail` and an `id` unique to the fill.

`/v1/tickers` lists every market once, in its canonical direction: each market
with a trade held, and each market with an order on the transparent book. Each
entry is that market's `ticker` plus `spark_7d`, the price at the end of each six
hours of the last seven days, oldest first (the last trade at or before each
point; 0 before the market's first trade). A markets list is one request.

Every rail's fills reach that log:

| rail | recorded by |
|---|---|
| `onchain-fill`, `covenant-round`, `clearing-round` | the chain watcher, from the filling transaction |
| `leaf-resting` | the resting follower, from the operator's view of the order |
| `interactive` | the lift session's settle ack, then the chain watcher |
| `cross`, `lightning` | the maker's signed settled-trade record |
| `leaf-maker`, `btc-gateway` | the maker itself, `POST /v1/trades/report` |

A fill attested twice (by its session and by the chain watcher, or a report sent
again) is one trade: a second record of an id the market holds is ignored. A
reorg that undoes a fill's transaction retracts its trade.

`POST /v1/trades/report` takes `{reporter, rail, id, give_asset, give,
want_asset, want, sig}`: the maker gave `give` atoms of `give_asset` (an asset id,
or `BTC`) for `want` atoms of `want_asset`. `sig` is a low-S DER ECDSA signature
(base64) by `reporter`'s key over
`sha256("seqob-trade-report-v1|reporter|rail|id|give_asset|give|want_asset|want")`.
The relay records it only when `reporter` is listed in `-trade-reporters` and the
rail is `leaf-maker` or `btc-gateway`; it answers `{recorded, id}`, `recorded`
false for a fill already held. A report is the maker's word: list only makers
whose trades the relay's operator vouches for.

Where several relays each serve some rails, the one the wallets read follows the
others with `-trade-follow`: it asks each `GET /v1/trades` every three seconds and
shows their trades with its own, one per id; a trade a followed relay retracts is
gone at its next answer. Follow only transparent relays: a confidential relay's
trades stay off the public view.

With `-node-rpc`, a covenant offer is admitted only when its outpoint is
unspent, pays the program its signed terms derive, holds asset A explicitly,
and holds at least `min_lot` and at most `offer_amount`; the relay lists the
order at what the outpoint holds. The outpoint is outside the maker's
signature, so after a part fill (the order rests again at the fill's output
2k+1) the same signed offer can be posted again naming the remainder, by its
maker or anyone holding it, and is listed at the remainder's size. That is how
a covenant order returns to the book after a relay restart.

The book lists one offer per covenant outpoint: two offers naming one coin
would count it twice. An offer from the same maker created later than the one
listed at its outpoint replaces it (the listed one leaves the book, CANCELLED);
one created at the same time or earlier is refused, and so is any other
maker's offer for a listed outpoint (409 `covenant outpoint already listed`).
The rule holds wherever an order enters the book or moves: a post, an edit, a
re-rest after a part fill, a reorg's undo and a read-back of the covenant book.

**Orders resting in an operator's tree.** An order can rest as an `order-3`
leaf (or an `order-2` leaf from before it) in the tree of an Arca operator the relay follows (`-arca-operator`),
under the offer member `RestingLeafTerms`. Its maker rests it and leaves;
takers fill it through the operator (`POST <operator>/v1/order_fill`), never
through the relay, and each fill is co-signed by the operator and held to the
order's terms by the leaf's program. The relay's evidence is the operator's
state view (`POST /v1/order_state`), so:

- it admits such an offer only when the operator, one it follows and on the
  relay's chain, has the offer's `operator_key` and reports the order
  `resting` at the offer's locator (`coin`) holding exactly its `value`,
  under exactly the offer's terms, and the order is before its delist time.
  Otherwise the post is refused, naming why: the operator reports no such
  order, reports it at another coin or holding another value, reports other
  terms, or reports it `cancelled`, `filled` or otherwise not resting; or the
  delist time has passed. The order is listed at its coin's value, never
  above the `offer_amount` its maker signed;
- every `-resting-interval` it asks the view again (one read per operator, up
  to 100 orders each). A fill moves a part-filled order to a new coin under a
  new salt: the relay moves the listing there and lists it at what that coin
  holds, the signed offer unchanged (the locator is outside the signature). It
  delists the order (`FILLED`, `CANCELLED` or `EXPIRED`) when the operator
  reports it filled whole, cancelled, used up (`exhausted`: its four credit
  keys spent), not live (landed on the chain, or gone), or no longer has it,
  and at its delist time: the earlier of the landed order's REFUND time less
  the operator's order margin (six hours) and the exit deadline of the batches
  its coin rests on (three days before their first expiry), "now" being the
  later of the relay's clock and the chain's median time. While the operator
  cannot be asked, its orders are listed at 0, and so is an order whose coin is
  not live yet (`pending`) or that the operator shows `filled` by a part fill
  whose remainder it does not hold yet: neither can be filled until the operator
  names the coin it rests at. An order that is filled, cancelled or otherwise done
  while a fill of it is recorded at the operator and not signed is listed at 0 until
  it is signed, so that fill is recorded before the order leaves the book;
- it records each signed fill the view reports, keyed by the operator's
  transfer id (an in-tree fill has no txid): the coin spent, what it held,
  what was filled, the maker's credit, the remainder, and as taker
  `operator_tree` with the operator's key and the round (and batch output) the
  order's first coin rests in. The fill is `confirmed` while that round is in
  a block of the active chain, `final` once it is final as a covenant fill is
  (certified, anchor buried `-fill-anchor-depth`), and `undone` if the round
  leaves the active chain, its trade retracted; it is recorded again if the
  round comes back.

The book lists one offer per resting order, by operator key and order id, as
it lists one per covenant outpoint: the same maker's later offer replaces the
listed one, and any other is refused (409 `resting order already listed`). A
lift of a resting order is refused: it is filled through its operator.

**Pools and round orders.** A maker's pool (`PoolTerms`, the template
`seqdex/pool`) posts two offers, both `TRADE_DIR_SELL` and both carrying the
pool's terms and its two coins: its ask, offering the base for the quote at
the pool's ask, and its bid, offering the quote for the base at its bid
(listed on the reversed market). The relay derives the pool's script from the
terms, requires both coins unspent at it holding the pool's assets explicitly,
and lists the ask at what the base coin holds above `reserve_base` and the bid
at what the quote coin holds above `reserve_quote`, never above the
`offer_amount` its maker signed: a pool lists only what it holds. Refused at a
post: another template, a price that is not the side's quote, a coin not at
the pool's script or spent, a side with nothing above its reserve. The chain
watcher follows the pool every pass: a fill re-creates it at the spender's
outputs `2j` and `2j+1` and both offers move there; while a spend of the pool
is unconfirmed both are held at 0; the maker's withdrawal holds them at 0 and
delists them once it is final; a move whose block leaves the chain is undone
and the pool listed again at the coins it rested at before. A pool is filled
on the chain by the pool's own fill, in or out of a clearing round, never by a
lift (refused).

A round order (`RoundOrderTerms`, `seqdex/round-order`) is listed like a
Simplicity covenant order, at what its coin holds and at its limit, and
followed the same way to its remainder at `2k+1`; it fills only in a clearing
round its clearer signs, so a lift of it is refused and the matcher crosses
neither it nor a pool. Each fill of one is recorded with the taker
`clearing_round`, the clearer's key and the price the round's last output
publishes, and `/v1/rounds` groups those records by round with the pool moves
of the same transaction: `confirmed`, `final` once certified with its anchor
buried, `undone` when the round's block leaves the chain, and recorded again
when it comes back. `seqob-covenant round` lays a round out from these
listings.

**The book across a restart.** The relay keeps its covenant orders, both sides
of each maker's pool, and the orders resting in operators' trees, and only
those, when given `-covenant-book`; a resting order is checked again against its operator's
state view when read back, as a post is. A covenant order's maker is offline by
design and its coin waits on the chain, so after a restart nobody may be there
to post it again, and the order would sit unlisted until its refund. The file
holds only public text: each order's signed offer at its current outpoint,
with its listed size. Nothing in it is trusted when it is read back: each
order goes through the checks a posted offer does (signature, terms, expiry,
the outpoint on the chain) and is listed at what its outpoint holds. An order
whose outpoint was spent while the relay was down is held at 0, and the chain
watcher follows the spend: to the remainder a part fill made, or out of the
book. A pool's side is kept at the two coins the pool rests at; if a fill
spent them while the relay was down, both sides are held at 0 and the pool
follower moves them to the coins the fill re-created the pool at (or delists
them once the maker's withdrawal is final), and a side sold down to its
reserve comes back held at 0, as it was. The fill and pool-move records
(`/v1/rounds`, `/v1/pools/moves`) are not in the file: after a restart they
hold what the relay has seen since it started. The file is rewritten, atomically, within two seconds of a change, and
as the relay stops. Interactive, cross-chain and Lightning offers are not
kept: their makers or agents post them again when they reconnect, and keeping
an interactive offer whose maker is gone would list an order nobody can fill.

Every book the relay serves (the orderbook, a maker's own orders, the
WebSocket snapshot) is a `PublicBook`: the makers' signed offers byte for
byte, and beside each one, at the same index in `statuses`, its
`OrderStatus`, whose `active_amount` is what the order can be filled for now.
That is the size to show: after a part fill a covenant order holds less than
its signed `offer_amount`, a pool's side holds what its coin does above its
reserve, and while a fill of either is unconfirmed it is 0.

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
- A Simplicity order (`SimplicityCovenantTerms`) is reconciled the same way:
  its outpoint must pay the script its terms derive under the version of
  `seqdex/order` its template hash names (2, or 1 for an order from before
  version 2; `seqob-cli book` prints which), and a spend's witness
  says whether it was a partial fill, a whole fill or the refund key's spend
  (no fill, no trade). A whole fill's `2k+1` is whatever the taker put there.
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
scriptPubKey, and refuses a `rate_num`, `rate_den` or `min_lot` of 2^63 or
more: the FILL leaf's signed 64-bit opcodes would read it as negative and fill
the order for any credit (the relay's validator refuses it too, and an order
that needs such a term is a Simplicity order, `simderive`); `fill` prints the permissionless FILL spend of an order at input
`k` (credit index `2k`, remainder index `2k+1`, witness, control block);
`filltx` lays out a transaction filling several orders, order `k` at input
`k`, from a JSON spec on stdin, and runs every order's leaf over it; `check`
judges a transaction as an order's FILL leaf at input `k` would, reading the
raw transaction with `covenant.ReadOutputs` rather than go-elements, which
cannot read a Sequentia asset issuance (an Arca round issues its batch token). The scripts
are byte-identical to the Python builder in `../test/regtest`, pinned by
`pkg/covenant/leaf_test.go`; the multi-order layout is pinned by
`pkg/covenant/testdata/fill_vectors.json`, which the web wallet's
`covenant.js` reproduces byte for byte.

`simderive`, `simwitness` and `simcheck` do the same for a Simplicity order of
the template `seqdex/order` (`pkg/covenant` `SimOrder`): its address from the
terms and the pinned commitment root of version 2, or of version 1 with
`-template-version 1`; the witness stack of a partial fill, a
whole fill or (given the refund key's signature over the transaction's
`sig_all_hash`) the refund, from the template's pinned programs; and a
judgement of a fill at input `k` as the program makes it. `-maker` is the
maker's whole payout scriptPubKey, which the order commits to by its hash.
`pkg/covenant/simorder_test.go` checks the address against every vector of
both template copies, `pkg/covenant/seqdex_order_2/` and
`pkg/covenant/seqdex_order/`, each of which its `PIN.json` ties to a commit of
sequentia-contracts.

The pool and the clearing round have their own subcommands, each reading a
JSON spec on stdin (`cmd/seqob-covenant/pool_round.go` documents the specs).
A pool (`pkg/covenant` `SimPool`, template `seqdex/pool`) is a maker's two
coins, one of each asset of a market, that anyone fills from either side at
its bid and ask, never below the reserve the maker states, and that only the
maker's key withdraws. A round order (`RoundOrder`, template
`seqdex/round-order`) fills only in a round its clearer signs, at the one price
the round publishes in its last output, credited exactly at that price and
never below its limit. `poolderive` and `roundderive` give an instance's
address; `poolfill` lays out a fill of a pool at its quotes; `round` chooses a
round's price over its round orders and pools (`ClearPrice`: the most base
exchanged, then the least left unmatched, then the lowest price), fills them
(`Allocate`), lays the round out (`BuildRound`) and signs it as the clearer;
`roundsign` signs any given transaction as the clearer; `poolwithdraw` and
`roundrefund` sign a withdrawal or a refund over each input's Simplicity
`sig_all_hash`, which `pkg/covenant` computes itself (`SimSigAllHash`,
`SimTxHash`, `ClearingDigest`, held to the contracts toolchain by
`pkg/covenant/testdata/simhash_vectors.json`). The template copies in
`pkg/covenant/seqdex_pool/` and `seqdex_round_order/` are tied to a commit of
sequentia-contracts by their `PIN.json`.

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
                           rate limits, replay gate, the liveness probe
                           (covenant orders, with -node-rpc: watcher's chain
                           probe, which also gives the size an order is
                           listed at)
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
internal/seqob/covfill     covenant fill construction for takers (one or several orders,
                           tapscript or Simplicity)
internal/seqob/settler     joint settlement of two covenant orders
internal/seqob/watcher     covenant book reconciliation against the chain
internal/seqob/bridge      cross-rail (covenant vs Lightning) settlement
internal/seqob/reorg       Bitcoin-anchor reorg detection helpers
pkg/covenant               the covenant leaf builders: the tapscript FILL/REFUND
                           leaves (NUMS key, output key, fill planning) and the
                           Simplicity order (SimOrder: address, witnesses,
                           fill planning and judgement, no compiler); the
                           pool (SimPool) and the round order (RoundOrder),
                           the clearing rule and the round's layout, and
                           Simplicity's tx_hash and sig_all_hash
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
