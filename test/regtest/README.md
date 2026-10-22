# SeqOB regtest proofs

Consensus-level proofs of the passive order book, run against a real Sequentia
node on regtest. They live here, not in the node repo, because what they test is
**this** repo's code: the covenant builder (`daemon/pkg/covenant`), the `seqobd`
relay and its matcher, the settler, the chain-watcher, and the rail-crossing
bridge. The node is the environment they run in, not the subject.

| Proof | What it establishes |
|---|---|
| `feature_seqob_covenant_fill.py` | The FILL/REFUND covenant itself: 11 scenarios against the script interpreter, no Go, no relay. An offline maker's terms are enforced by consensus alone. |
| `feature_seqob_matcher_covenant.py` | The relay's matcher crosses a covenant-funded resting order against an incoming one, and it settles with the maker offline. |
| `feature_seqob_joint_covenant.py` | Two covenant orders, **both** makers offline, settled against each other in one transaction by an untrusted settler that holds no funds. |
| `feature_seqob_watcher.py` | The chain-watcher reconciles the keyless relay's book to chain state: full fill, partial re-rest, unconfirmed, and the ghost-funding case a Bitcoin-driven reorg creates. |
| `feature_seqob_multi_fill.py` | Several orders filled in one transaction, order `k` at input `k`, laid out by `pkg/covenant`'s planner; each wrong layout (credits swapped, a credit one atom short, a gap in the sold asset, a rest to another script or below `min_lot`) forced into a block and refused for the script failure `CheckFill` names. |
| `feature_seqob_simplicity_order.py` | The Simplicity order (`pkg/covenant` `SimOrder`, template `seqdex/order` version 2) with Simplicity active from genesis: Go's address is the node's script for an order beyond the tapscript leaf's 2^63 bound; that order filled in part at `k=0`, again at `k=1` and whole with the witnesses Go builds from the pinned programs, judged first by `simcheck`; a Simplicity order and a tapscript order filled in one transaction; a credit one atom under the price, a rest under other terms and the wrong index forced into a block and refused; each fill's size against a tapscript fill in the same layout. |
| `feature_seqob_simplicity_book.py` | A Simplicity order in the book, Simplicity active from genesis: posted with `seqob-relaycli` and served after the validator and the chain probe accept it (a post naming another template, and one whose outpoint holds more than `offer_amount`, refused for their reasons); filled in part by `seqob-cli covfill`, recorded by the watcher from the fill's outputs and served at `(txid, 1)`; filled whole, held until final and removed; both trades in the feed. An order under `seqdex/order` version 1, as orders resting from before version 2 are, is listed (`seqob-cli book` names its version) and filled whole by `covfill`; an offer signing version 2's template hash for that coin is refused, the coin not paying the program its signed version derives. |
| `feature_seqob_simplicity_annex.py` | The two versions of `seqdex/order` on an anchored chain (`-validateanchor=1`), every address and witness from SeqDEX's Go: each version's address is the node's script, and one order's terms give the two versions two addresses; a version 2 partial fill with an annex of 3 and of 1,000 bytes, or a zero one of 1,000, on the order's input is refused by the mempool (for the program's failure, or on a node from Sequentia 25.2.4 for the annex itself) and forced into a block, refused there for the program's failure (`Assertion failed`), beside the same fill without one, which is mined; version 1's fill with a 1,000-byte annex is mined in a forced block (the program takes an annex nobody signed; a node before 25.2.4 also relays it), and the version 1 order's remainder then fills whole. |
| `feature_seqob_book_remainder.py` | A part-filled covenant order across relay restarts, on an anchored chain: a tapscript order filled in part rests at the fill's output 1; after a restart its maker's signed offer, unchanged, posted again naming that remainder is listed at the remainder's size; the same signed offer naming an outpoint of another program, one holding more than `offer_amount`, one below `min_lot`, a confidential one and a spent one is refused for its reason, and one holding exactly `min_lot` is listed; the re-listed order filled in part and whole. At every step the orderbook serves, beside the signed offer, its status, whose `active_amount` is the outpoint's value (0 while a fill is unconfirmed). With `-covenant-book` the relay lists the order again by itself after a restart; one filled while the relay was down is followed to its new remainder and its fill recorded; one that expired meanwhile is dropped by name. One offer per outpoint: the maker's later offer for an outpoint replaces its earlier one, which posted again is refused, as is another maker's offer for it; a part fill re-rests it once; a covenant book with two lines for one outpoint lists one. `SEQOBD_BIN` runs it against another relay build. |
| `feature_seqob_bridge.py` | An on-chain covenant order crossed against a Lightning order, settled under one preimage by the bridge. On-chain leg live; LN leg mocked at the orchestrator seam. |

`seqob_covenant.py` is the Python covenant builder the first proof validates, and
which `daemon/pkg/covenant` is a byte-for-byte port of. `gen_refund_golden.py`
emits the deterministic REFUND vector SWK's Rust golden test asserts against.
The design they implement is [`docs/simplicity-dex-covenant-offers-design.md`](../../docs/simplicity-dex-covenant-offers-design.md).

## Running them

They need a **configured, built** [Sequentia node](https://github.com/ConcatenaLabs/Sequentia)
checkout, whose `test_framework` package they import and whose `test/config.ini`
points at the built binary. Everything but `feature_seqob_covenant_fill.py` also
needs the Go toolchain, and builds the daemon binaries it drives from this repo.

    SEQUENTIA_DIR=~/Sequentia python3 test/regtest/feature_seqob_covenant_fill.py

`seqob_env.py` resolves the checkouts and does the `sys.path` wiring; import it
before any `test_framework` import. It reads:

| Variable | Default |
|---|---|
| `SEQUENTIA_DIR` | `~/Sequentia` — a configured, built node checkout |
| `SEQDEX_DIR` | this repo, derived from the test's own path |
| `GO_BIN` | `~/dev-tools/go/bin/go` |

A missing Go toolchain or daemon tree is a `SkipTest`, not a failure. A missing
node checkout is a hard exit: without it there is nothing to prove anything
against.
