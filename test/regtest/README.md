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
| `feature_seqob_simplicity_order.py` | The Simplicity order (`pkg/covenant` `SimOrder`, template `seqdex/order`) with Simplicity active from genesis: Go's address is the node's script for an order beyond the tapscript leaf's 2^63 bound; that order filled in part at `k=0`, again at `k=1` and whole with the witnesses Go builds from the pinned programs, judged first by `simcheck`; a Simplicity order and a tapscript order filled in one transaction; a credit one atom under the price, a rest under other terms and the wrong index forced into a block and refused; each fill's size against a tapscript fill in the same layout. |
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
