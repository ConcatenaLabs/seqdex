# A cross-chain swap lock that dies with its bitcoin payment

This is the proven design for a swap lock without a rule change. It is kept as a record and is
not built: the chosen design is a node-checked proof of the bitcoin payment.

It specifies a lock for a swap of a Sequentia asset against on-chain bitcoin (swap version 2)
that every implementation of that swap could follow: SeqDEX's cross-chain drivers, the node
wallet's convert, the wallet kit and the web wallet. Its scripts and their attacks are proven
on regtest by `daemon/pkg/xchain/namedlock_regtest_test.go`.

## The problem it solves

Every Sequentia block names a Bitcoin block, and when Bitcoin reorganizes, Sequentia
reorganizes with it. The transactions of the blocks it drops go back to the mempool and are
mined again in the next block. A plain hash-locked swap does not survive that:

- **The buyer's attack.** The buyer pays bitcoin, waits for the asset to be locked, claims it,
  then reorganizes Bitcoin from the block of its payment and spends the same coin to itself.
  Sequentia drops the lock and the claim, then mines both again. The buyer keeps its bitcoin
  and takes the asset.
- **The seller's attack.** After any Bitcoin reorganization that drops the block the lock
  anchors to while the bitcoin payment stays, the seller replaces its own lock in the mempool.
  The buyer's claim falls with it, but the secret is already public, so the seller takes the
  bitcoin and keeps the asset. A routine one- or two-block reorganization is enough.

The lock below makes the Sequentia side follow the bitcoin payment: if the payment is
double-spent, the seller takes the asset back; if the payment stays, the seller cannot pull
the lock away. No consensus rule is involved, and the buyer claims in the block after the
lock, as before.

## Who is who

- The **buyer** pays bitcoin and receives the asset. It holds the secret `s` and its hash
  `H = SHA256(s)`. In the forward direction the buyer is the taker; in the reverse
  direction it is the maker.
- The **seller** locks the asset and receives the bitcoin.

Keys:

| Key | Holder | Signs |
|---|---|---|
| `Pb` | buyer | the key of the bitcoin coin `C` the buyer already holds; it signs the bitcoin payment `F` and never a second spend of `C` |
| `Bh` | buyer, fresh | the lock transaction `L` (its half of the holding output) |
| `Bc` | buyer, fresh | the claim |
| `Bo` | buyer, fresh | the later spend of the claim's output |
| `Sh` | seller, fresh | the lock transaction `L` (its half of the holding output), or the holding output's return after `Th` |
| `Sc` | seller, fresh | the claim, once, at a fixed fee |
| `Sx` | seller, fresh | the cancel and the refund |

`Pb` is whatever key holds the buyer's coin. In a dual-chain wallet that is also the key of
the wallet's Sequentia address, and it may sign anything else on either chain: Sequentia
transactions, messages, spends of other coins at the same address. The cancel counts only a
second spend of `C` itself.

`Sc` and `Sx` must differ. A seller signature over the claim transaction made with the cancel
key would let the buyer spend the lock through the cancel branch with a proof of its own and
take the asset without revealing the secret.

## The three parts

1. **The lock names the bitcoin payment.** The bitcoin payment `F` spends exactly one coin
   `C`, a P2WPKH output under `Pb`, signed with `SIGHASH_ALL`. The lock has a cancel branch
   that pays the asset back to the seller when shown a signature by `Pb` over a Bitcoin
   transaction that spends `C` and is not `F`: a double-spend of the payment, and nothing else.
2. **The claim cannot share a block with its lock.** The claim needs `1 OP_CHECKSEQUENCEVERIFY`.
   The buyer already claims in a later block than the lock's, because it waits for the lock's
   block to pass its anchor check, so the normal flow waits for nothing more. After a
   reorganization the lock is mined again first, and the seller's cancel can reach the next
   block before the claim can.
3. **The lock spends a holding output.** The lock's input is a two-signature output (`Sh` and
   `Bh`, or `Sh` alone after `Th`) that was confirmed before the bitcoin payment. The buyer
   signs only the lock transaction with `Bh`, so once the lock is back in the mempool after a
   reorganization, nothing the seller can sign spends that input elsewhere.

And one rule across them: **no transaction of a swap signals replaceability** except the
claim, whose `nSequence` of 1 signals by itself. That signal is what lets a late cancel replace
a claim waiting in the mempool.

## How the cancel recognizes a double-spend

A Bitcoin signature over an input spending a P2WPKH coin signs the double SHA256 of a
182-byte BIP143 preimage:

| Bytes | Field |
|---|---|
| 0–3 | version |
| 4–67 | hashPrevouts, hashSequence |
| 68–137 | the outpoint, the script code `76a914 HASH160(Pb) 88ac`, the amount |
| 138–181 | nSequence, hashOutputs, nLockTime, hash type |

Bytes 68–137 are the same for every spend of `C`, and the lock carries them. The cancel's
witness carries the rest in four pieces: bytes 0–3 (the version), 4–24, 25–67 and 138–181.
The script checks each piece's size, refuses a preimage whose first 25 bytes are the
signed-message prefix (below), joins the parts with `OP_CAT` (enabled in segwit v0 on
Sequentia, with the 520-byte element limit), hashes them once with `OP_SHA256`, requires the
result to differ from `m0` (the same value for `F`), and checks `Pb`'s signature over it with
`OP_CHECKSIGFROMSTACKVERIFY`, which hashes once more. The signature therefore verifies only if
`Pb` signed a Bitcoin spend of `C`.

Nothing else `Pb` signs fits:

- **A spend of another coin** at the same address names another outpoint at bytes 68–103.
- **A Sequentia transaction** has another preimage layout (it adds the issuance hash and
  asset commitments, and is at least 215 bytes), so its signed hash cannot equal the hash of
  182 bytes with `C` at offset 68 short of a SHA256 collision; Sequentia's taproot signatures
  are BIP340 and never pass this check.
- **A signed message** (Bitcoin's and Sequentia's `signmessage`) begins with the 25 bytes
  `\x18Bitcoin Signed Message:\n`, and the cancel refuses a preimage that begins with them.
  A transaction's preimage cannot: bytes 4–24 are hash output. So even a message crafted to
  carry `C`'s outpoint, script code and amount at offset 68 is not a proof.
- **A BIP322 proof of funds** that lists `C` among its inputs is a signature over a BIP143
  preimage spending `C`, and does count. A buyer never makes one for `C` while a swap on it
  is open; after `F` is mined `C` is spent and no proof of funds can name it.
- A wallet that signs arbitrary 32-byte hashes with its coin keys can be made to sign
  anything, and is outside what this lock defends.

A double-spend therefore proves itself under every signature-hash type and every version,
including the non-standard ones a pool may mine for a fee.

## The fixed claim

The claim is a transaction the seller signs once, at swap time, with `Sc`. Its outputs and fee
are fixed, so the buyer cannot raise the claim's fee to outbid the cancel. Its only payment
output is locked for one block (`<Bo> OP_CHECKSIGVERIFY 1 OP_CHECKSEQUENCEVERIFY`), so nothing
can spend it while it is unconfirmed, and the buyer cannot pin the claim in the mempool with a
high-fee child. A cancel paying more than the claim's fixed fee therefore always replaces it,
and the fee race ends there. The buyer's wallet moves the claimed asset to its own address in
any later transaction.

The fee cap could also have been written with introspection opcodes, but those exist only in
tapscript, where `OP_CHECKSIGFROMSTACK` checks a BIP340 signature. That would require every
buyer's bitcoin coin to be a key-path taproot output. A signature over a fixed transaction
gives the same bound and works with the P2WPKH coins every wallet holds.

If the fixed claim cannot be mined (the asset stopped being accepted for fees, or the network
is too busy for its fee), the buyer can claim alone after `k2` blocks (5 by default), at any
fee, through the late branch. By then a seller who needed to cancel has had `k2` blocks to do so.

## The scripts

The holding output, P2WSH:

```
OP_IF
    2 <Sh> <Bh> 2 OP_CHECKMULTISIG
OP_ELSE
    <Th> OP_CHECKLOCKTIMEVERIFY OP_DROP <Sh> OP_CHECKSIG
OP_ENDIF
```

The lock, P2WSH. `K` is bytes 68–137 of every preimage spending `C`; `m0` is `SHA256` of
`F`'s preimage.

```
OP_IF                                       # claim
    OP_SHA256 <H> OP_EQUALVERIFY
    1 OP_CHECKSEQUENCEVERIFY OP_DROP
    <Sc> OP_CHECKSIGVERIFY
    <Bc> OP_CHECKSIG
OP_ELSE
  OP_IF                                     # cancel: sig(Sx) sig(Pb) b0_3 b4_24 b25_67 b138_181
    OP_SIZE 44 OP_EQUALVERIFY OP_TOALTSTACK
    OP_SIZE 43 OP_EQUALVERIFY OP_TOALTSTACK
    OP_SIZE 21 OP_EQUALVERIFY
    OP_SWAP OP_SIZE 4 OP_EQUALVERIFY OP_SWAP OP_CAT
    OP_DUP <18 "Bitcoin Signed Message:\n"> OP_EQUAL OP_NOT OP_VERIFY
    OP_FROMALTSTACK OP_CAT
    <K> OP_CAT
    OP_FROMALTSTACK OP_CAT
    OP_SHA256
    OP_DUP <m0> OP_EQUAL OP_NOT OP_VERIFY
    <Pb> OP_CHECKSIGFROMSTACKVERIFY
    <Sx> OP_CHECKSIG
  OP_ELSE
    OP_IF                                   # late claim
      OP_SHA256 <H> OP_EQUALVERIFY
      <k2> OP_CHECKSEQUENCEVERIFY OP_DROP
      <Bc> OP_CHECKSIG
    OP_ELSE                                 # refund
      <T> OP_CHECKLOCKTIMEVERIFY OP_DROP
      <Sx> OP_CHECKSIG
    OP_ENDIF
  OP_ENDIF
OP_ENDIF
```

Witnesses, bottom of the stack first:

| Branch | Witness |
|---|---|
| claim | `sig(Bc)`, `sig(Sc)`, `s`, `01` |
| cancel | `sig(Sx)`, `sig(Pb)` (DER, low-S, no hash-type byte), preimage bytes 0–3, 4–24, 25–67 and 138–181, `01`, empty |
| late claim | `sig(Bc)`, `s`, `01`, empty, empty |
| refund | `sig(Sx)`, empty, empty, empty |

The claim's output, P2WSH: `<Bo> OP_CHECKSIGVERIFY 1 OP_CHECKSEQUENCEVERIFY`.

The bitcoin side is a hash-locked P2WSH output that pays the seller with `s`, or the buyer
after `T_btc`. Version 2 uses P2WSH on both sides of every implementation: its claim is less
than half the size of a legacy P2SH claim, which matters whenever the claim must be
re-submitted after a reorganization.

## The transactions

| Transaction | Spends | Pays | Signed by | Broadcast by |
|---|---|---|---|---|
| `HF`, the holding output's funding | the seller's coins | the holding output | the seller, not replaceable | the seller |
| `F`, the bitcoin payment | `C` only | the bitcoin hash lock, and change | `Pb`, `SIGHASH_ALL` | the buyer |
| `L`, the lock | the holding output | the lock | `Bh` first, then `Sh`; `nSequence` final | the seller |
| the claim | the lock | the claim's output, at a fixed fee | `Sc` at swap time, then `Bc`; `nSequence` 1 | the buyer |
| the cancel | the lock | the seller | `Sx` at swap time; the proof added by whoever finds it | the seller or its watcher |
| the refund | the lock | the seller, `nLockTime` `T` | `Sx` | the seller or its watcher |
| the holding return | the holding output | the seller, `nLockTime` `Th` | `Sh` | the seller or its watcher |
| the late claim | the lock, after `k2` blocks | the buyer, at any fee | `Bc` | the buyer |

`F` spends a segwit input, so its txid is known before it is signed. `L` spends a segwit input
and is fully determined once the holding output exists, so the claim, the cancel, the refund
and the holding return can all be signed before anything else is broadcast.

## Timelines

### Forward (the taker buys the asset with bitcoin)

1. The taker asks for terms and names swap version 2, its keys, and its coin `C` (outpoint,
   amount and `Pb`).
2. The maker checks `C` (below), funds `HF`, and sends its terms: its keys, `T_btc`, `T`, `Th`,
   `k2`, and the holding output.
3. The taker waits for `HF` to confirm and checks it. It builds `F` without signing it,
   computes `m0`, builds `L` and signs it with `Bh`, and builds the claim. It sends `F`, its
   half of `L` and the claim.
4. The maker checks `F`, recomputes `m0` itself, checks that the lock and the claim are the
   agreed ones, signs the claim with `Sc` and sends that signature. It signs the cancel, the
   refund and the holding return and deposits them with its watcher.
5. The taker checks the maker's signature, signs `F` and broadcasts it, waits for its
   confirmations and announces it.
6. The maker checks that `F` (by its txid) is confirmed and that `C` was at least `D` blocks
   deep under it, waits for its node's anchor to reach `F`'s height, adds `Sh` to `L` and
   broadcasts it.
7. The taker waits for `L`'s block to pass the anchor check and broadcasts the claim in the
   next block, which reveals `s`.
8. The maker reads `s` from the claim and claims the bitcoin.

### Reverse (the taker sells the asset for bitcoin)

1. The taker asks for terms and names swap version 2 and its keys.
2. The maker sends its terms: `H`, its keys, `C` (outpoint, amount and `Pb`), `T_btc`, `T`,
   `Th` and `k2`.
3. The taker checks `C`, funds `HF` and announces it once confirmed.
4. The maker checks `HF`, builds `F` without signing it, computes `m0`, builds `L` and signs it
   with `Bh`, builds the claim, and sends `F`, its half of `L` and the claim.
5. The taker checks them, signs the claim with `Sc`, sends that signature, and deposits the
   signed cancel, refund and holding return with its watcher.
6. The maker signs and broadcasts `F`, waits for its confirmations and announces it.
7. The taker checks `F` and `C`'s depth, waits for its anchor, completes `L` and broadcasts it.
8. The maker passes the anchor check on `L` and claims, which reveals `s`; the taker reads `s`
   and claims the bitcoin.

In both directions the holding output adds one Sequentia block before the bitcoin payment is
broadcast, while `HF` confirms. The claim waits for nothing new.

## The holding output's timeout `Th`

`Th` must outlast the swap and the burial of the claim to the depth the seller covers, and
nothing more: past that point, a reorganization that brings the lock back is deeper than any
the swap protects against. In Sequentia blocks, counted from the tip when the terms are given:

- 60 blocks for the bitcoin payment to confirm and the anchor to pass it (about six Bitcoin
  blocks);
- 10 blocks for the claim (the claimant's margin);
- 10 blocks per Bitcoin block of the covered depth `D`, so 30 at `D = 3`.

So `Th` is the tip plus 100 blocks, about 100 minutes at one block a minute. The seller does
not complete `L` once the tip is within 40 blocks of `Th`; the swap then stops, the buyer's
bitcoin returns after `T_btc`, and nothing is lost. `T`, the lock's own refund, is set as
before by the claim window.

**When the other side walks away** after the holding output is funded, the seller's asset
comes back by itself at `Th`: the holding return is signed at swap time and its watcher
broadcasts it. A wallet shows this as a trade that did not go through, with the time the asset
is back ("Your 500 OIL is held for this trade and returns to you automatically by 14:20"),
and lists the asset in the balance with that note, never as lost.

**Makers' inventory.** A taker who asks for terms and walks away parks the maker's asset until
`Th` at no cost to itself. A maker therefore:

- accepts each buyer coin `C` in one swap at a time, checked unspent at the terms, so parking
  a lot costs the taker an unspent coin of the trade's size and age per lot;
- limits how much of its inventory sits in holding outputs at once, and how many swaps each
  counterparty and each relay session may hold open;
- refuses terms to a coin that has walked away from an earlier swap until that swap's `Th`.

## When the buyer has no single coin of the size and age

`F` spends exactly one coin, at least `D` blocks old. A wallet without one never refuses the
trade. In its default mode it chooses the way itself and says only how long it takes: another
route when one can carry the trade at once, otherwise it combines the coins into one and trades
once that has `D` confirmations ("This trade takes about 30 minutes."). In developer mode it
says why and lets the person choose.

A several-coin form fits the same script: one cancel sub-branch per named input, each with its
own fixed bytes `K_i` and its own `m_i`, and a cancel on any of them. Each further coin adds
about 130 bytes to the lock and makes the seller's checks per coin.

## The tapscript form

Restricted assets sit in taproot enclaves, and some buyers hold taproot bitcoin. For them the
lock is a tapscript leaf. There `OP_CHECKSIGFROMSTACK` checks a BIP340 signature over the
message as given, and the buyer's coin `C` must be a key-path-only taproot output: the buyer
shows its internal key, and the seller checks that the output key is that key tweaked with no
script tree, so every spend of `C` is a BIP340 signature by the output key. The signed message
is BIP341's `TapSighash` of the spending transaction, which commits to `C`'s outpoint through
`sha_prevouts` (or directly, under `ANYONECANPAY`). The cancel leaf rebuilds that message
around `C` with `OP_CAT` and Elements' streaming `OP_SHA256` opcodes (the witness gives the
other prevouts and the remaining fields) and checks the signature against the output key. In
tapscript the fixed claim can instead be a fee cap by introspection (the claim's outputs, their
asset and value, and the one-block payment script, read with `OP_INSPECTOUTPUT*`). Everything
else (the holding output, `1` CSV, the late claim, the refund) is the same.

## What each party checks before it signs

The seller, before it signs the claim:

- `C` is a P2WPKH output whose script is `0014 HASH160(Pb)`, unspent, of the amount named, and
  not named in another open swap.
- `F` has exactly one input, `C`, version 1, 2 or 3, and pays the agreed bitcoin hash lock its
  amount.
- `m0` is `SHA256` of `F`'s BIP143 preimage for that input, with hash type `SIGHASH_ALL`,
  computed by the seller itself.
- The lock, the claim and its fixed fee are the ones the scripts above give for the agreed keys
  and times, and the claim's fee is within the seller's cap.

The seller, before it completes `L`:

- `F`, by its txid, has the agreed confirmations, and `C` was confirmed at least `D` Bitcoin
  blocks below `F`. `D` is the depth of reorganization the seller wants covered (3 by default).
- Its node's anchor has reached `F`'s height, as before, and the tip is not within 40 blocks of
  `Th`.

The buyer, before it signs `F`:

- `HF` is confirmed, not replaceable, and pays the holding output the lock's amount and fee in
  the agreed asset.
- The seller's claim signature verifies over the fixed claim with `SIGHASH_ALL`.
- `T`, `T_btc`, `Th` and `k2` leave the claim window its usual margin.

The buyer's one rule: **never sign a second spend of `C`.** A replacement of `F` would be one,
and would hand the seller a valid cancel, so `F`'s fee is raised only by child-pays-for-parent
from its change; so would a BIP322 proof of funds naming `C`. Nothing else `Pb` signs matters.

## The watcher

An ordinary seller using a wallet runs nothing and need not stay online. At step 4 (forward)
or 5 (reverse) its wallet deposits with one or more watchers:

- the cancel, signed with `Sx`, paying the seller's own address at a fee above the claim's,
  lacking only the proof;
- the refund, complete, with `nLockTime` `T`, and the holding return, complete, with
  `nLockTime` `Th`;
- `C` (outpoint, amount, `Pb`), `m0`, `F` itself, and the bitcoin hash lock's script.

The watcher follows Bitcoin, its mempool as well as its blocks, and does three things:

1. **On any spend of `C` other than `F`**, it reads the proof from that spend (the `Pb`
   signature and the four preimage pieces), completes the cancel and broadcasts it on
   Sequentia, again every block until the lock is spent by it or by a claim that stays in a
   block. It does this first, before anything on Bitcoin.
2. **On every Bitcoin reorganization**, it sends `F` again, and the seller's bitcoin claim
   once it has seen it (on Bitcoin or in the mempool), until the claim is six blocks deep.
   After a reorganization nothing else re-submits them, and a payment left out of the new
   chain is the opening a double-spend needs.
3. It broadcasts the refund once the chain reaches `T` with the lock unspent, and the holding
   return once it reaches `Th` with the holding output unspent.

The watcher holds no key of the seller and takes no custody. Every output of what it broadcasts
is fixed by the seller's signature. What it can do wrong:

- **Not act.** If it is offline, or bribed, when the buyer double-spends, the seller loses the
  asset of that one swap; the buyer's claim gets in once the lock is mined again. The bound is
  the swap's value. Depositing with more than one watcher removes a single point of failure.
- **Act late by a block.** If the claim is already in a block, nothing can be done. If it is
  still in the mempool, the cancel replaces it, because the claim's fee is fixed and lower.
- **Learn the swap.** The deposit tells the watcher the swap's amounts and outputs.
- It cannot redirect the asset, cancel a swap whose payment stands (no second spend of `C`
  exists), or refund before `T`.

The watcher is a deposit kind of the watchtowers the Arca wallets already use (`arca
watchtower`), so a seller's wallet deposits where it already deposits its other coins. A maker
runs its own. Anyone can run one.

## Swap version

Version 2 is this lock. The taker names it in its terms request and the maker in its terms.
A maker that receives a request without it refuses with the code `swap_version` and the
message that the counterparty must update before they can trade; a taker that receives terms
without it refuses the same way. Nothing is funded or signed before both sides have named
version 2, so an older counterparty is refused, never half-served.

## What remains

- **Producers of two consecutive blocks who collude** with a buyer who reorganized Bitcoin and
  double-spent: one mines the lock again without the cancel, the next mines the claim.
- **A coin younger than the reorganization.** A reorganization below `C`'s own block lets the
  buyer spend `C`'s parent instead, which produces no `Pb` signature. `D` bounds this; the
  seller chooses it.
- **A reorganization below the holding output.** If Bitcoin reorganizes below the block that
  `HF`'s block anchors to, `HF` returns to the mempool too, and a producer who mines a
  respend of its inputs lets the seller withdraw the lock. `HF` does not signal replaceability,
  so honest mempools refuse that respend.
- **A watcher late by a block,** above.
- **A fee war on the Bitcoin side does not help.** It was tested against Bitcoin Core's default
  policy: a double-spend with two small descendants cannot be replaced by any package at any
  fee, and without them every answer costs the seller about 2.65 times the buyer's fee
  (P2WSH claim), so a double-spend opening at 40% of the payment cannot be answered even with
  all of it. The seller's defence is the cancel on Sequentia, not a bid on Bitcoin; a watcher
  never bids high for the bitcoin before it knows the cancel's outcome.
- **The seller's exposure before the payment,** bounded by `Th` and the maker's limits above.
