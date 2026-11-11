# A cross-chain swap lock that dies with its bitcoin payment

This is the specification of the lock SeqDEX uses for a swap of a Sequentia asset against
on-chain bitcoin (swap version 2). Every implementation of that swap follows it: SeqDEX's
cross-chain drivers, the node wallet's convert, the wallet kit and the web wallet.

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

Keys, all fresh for each swap:

| Key | Holder | Signs |
|---|---|---|
| `Pb` | buyer | the bitcoin payment `F`, and nothing else, ever |
| `Bh` | buyer | the lock transaction `L` (its half of the holding output) |
| `Bc` | buyer | the claim |
| `Bo` | buyer | the later spend of the claim's output |
| `Sh` | seller | the lock transaction `L` (its half of the holding output), or the holding output's return after `Th` |
| `Sc` | seller | the claim, once, at a fixed fee |
| `Sx` | seller | the cancel and the refund |

`Sc` and `Sx` must differ. A seller signature over the claim transaction made with the cancel
key would let the buyer spend the lock through the cancel branch, which it can satisfy with its
own `Pb`, and take the asset without revealing the secret.

## The three parts

1. **The lock names the bitcoin payment.** The bitcoin payment `F` spends exactly one coin
   `C`, a P2WPKH output under `Pb`, and `Pb` signs it once with `SIGHASH_ALL`. Every
   transaction other than `F` that spends `C` must carry a new `Pb` signature over a different
   message. The lock has a cancel branch that pays the asset back to the seller when shown a
   `Pb` signature over any message other than `m0`, the one `F` was signed over.
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
is too busy for its fee), the buyer can claim alone after `k2` blocks, at any fee, through the
late branch. By then a seller who needed to cancel has had `k2` blocks to do so.

## The scripts

The holding output, P2WSH:

```
OP_IF
    2 <Sh> <Bh> 2 OP_CHECKMULTISIG
OP_ELSE
    <Th> OP_CHECKLOCKTIMEVERIFY OP_DROP <Sh> OP_CHECKSIG
OP_ENDIF
```

The lock, P2WSH. `m0` is `SHA256` of the BIP143 preimage of `F`'s only input, so that
`OP_CHECKSIGFROMSTACKVERIFY`, which hashes its message once more with `SHA256`, checks exactly
the signature hash Bitcoin checks.

```
OP_IF                                       # claim
    OP_SHA256 <H> OP_EQUALVERIFY
    1 OP_CHECKSEQUENCEVERIFY OP_DROP
    <Sc> OP_CHECKSIGVERIFY
    <Bc> OP_CHECKSIG
OP_ELSE
  OP_IF                                     # cancel
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
| cancel | `sig(Sx)`, `sig(Pb)` without its hash-type byte, `data`, `01`, empty |
| late claim | `sig(Bc)`, `s`, `01`, empty, empty |
| refund | `sig(Sx)`, empty, empty, empty |

The claim's output, P2WSH: `<Bo> OP_CHECKSIGVERIFY 1 OP_CHECKSEQUENCEVERIFY`.

The bitcoin side is unchanged: a hash-locked output that pays the seller with `s`, or the buyer
after `T_btc`.

## The transactions

| Transaction | Spends | Pays | Signed by | Broadcast by |
|---|---|---|---|---|
| `HF`, the holding output's funding | the seller's coins | the holding output | the seller, not replaceable | the seller |
| `F`, the bitcoin payment | `C` only | the bitcoin hash lock, and change to a fresh address | `Pb`, `SIGHASH_ALL` | the buyer |
| `L`, the lock | the holding output | the lock | `Bh` first, then `Sh`; `nSequence` final | the seller |
| the claim | the lock | the claim's output, at a fixed fee | `Sc` at swap time, then `Bc`; `nSequence` 1 | the buyer |
| the cancel | the lock | the seller | `Sx` at swap time; the `Pb` proof added by whoever finds it | the seller or its watcher |
| the refund | the lock | the seller, `nLockTime` `T` | `Sx` | the seller or its watcher |
| the late claim | the lock, after `k2` blocks | the buyer, at any fee | `Bc` | the buyer |

`F` spends a segwit input, so its txid is known before it is signed. `L` spends a segwit input
and is fully determined once the holding output exists, so the claim, the cancel and the refund
can all be signed before anything else is broadcast.

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
   agreed ones, signs the claim with `Sc` and sends that signature. It signs the cancel and the
   refund and deposits them with its watcher.
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
   signed cancel and refund with its watcher.
6. The maker signs and broadcasts `F`, waits for its confirmations and announces it.
7. The taker checks `F` and `C`'s depth, waits for its anchor, completes `L` and broadcasts it.
8. The maker passes the anchor check on `L` and claims, which reveals `s`; the taker reads `s`
   and claims the bitcoin.

In both directions the holding output adds one Sequentia block before the bitcoin payment is
broadcast, while `HF` confirms. The claim waits for nothing new.

## What each party checks before it signs

The seller, before it signs the claim:

- `C` is a P2WPKH output whose script is `0014 HASH160(Pb)`, unspent, and of the amount named.
- `F` has exactly one input, `C`, and pays the agreed bitcoin hash lock its amount.
- `m0` is `SHA256` of `F`'s BIP143 preimage for that input, with hash type `SIGHASH_ALL`,
  computed by the seller itself.
- The lock, the claim and its fixed fee are the ones the scripts above give for the agreed keys
  and times, and the claim's fee is within the seller's cap.

The seller, before it completes `L`:

- `F`, by its txid, has the agreed confirmations, and `C` was confirmed at least `D` Bitcoin
  blocks below `F`. `D` is the depth of reorganization the seller wants covered.
- Its node's anchor has reached `F`'s height, as before.

The buyer, before it signs `F`:

- `HF` is confirmed, not replaceable, and pays the holding output the lock's amount and fee in
  the agreed asset.
- The seller's claim signature verifies over the fixed claim with `SIGHASH_ALL`.
- `T`, `T_btc`, `Th` and `k2` leave the claim window its usual margin.

The buyer's discipline, which only it can keep and which only harms it if broken:

- `Pb` signs `F` and nothing else, ever: not another coin at the same address, not a fee bump.
  A replacement of `F` would hand the seller a valid cancel. A fee bump is by child-pays-for-
  parent from `F`'s change, which is at another key.
- `C`'s address is not reused. A coin that arrives there later stays unspent.

## The watcher

An ordinary seller using a wallet runs nothing and need not stay online. At step 4 (forward)
or 5 (reverse) its wallet deposits with one or more watchers:

- the cancel transaction, signed with `Sx`, paying the seller's own address at a fee above the
  claim's, lacking only the `Pb` signature and its message;
- the refund transaction, complete, with `nLockTime` `T`;
- `C` (outpoint, amount, `Pb`), `m0` and `F`'s txid.

The watcher watches Bitcoin, mempool and blocks, for a spend of `C` that is not `F`. When one
appears it takes that input's `Pb` signature, computes the message from the spending
transaction, checks that it differs from `m0` and verifies, completes the cancel and broadcasts
it on Sequentia every block until the lock is spent by it or by a claim that has stayed in a
block. It broadcasts the refund once the chain reaches `T` with the lock unspent.

The watcher holds no key of the seller and takes no custody. Every output of what it broadcasts
is fixed by the seller's signature. What it can do wrong:

- **Not act.** If it is offline, or bribed, when the buyer double-spends, the seller loses the
  asset of that one swap; the buyer's claim gets in once the lock is mined again. The bound is
  the swap's value. Depositing with more than one watcher removes a single point of failure.
- **Act late by a block.** If the claim is already in a block, nothing can be done. If it is
  still in the mempool, the cancel replaces it, because the claim's fee is fixed and lower.
- **Learn the swap.** The deposit tells the watcher the swap's amounts and outputs.
- It cannot redirect the asset, cancel a swap whose payment stands (no `Pb` signature over
  another message exists), or refund before `T`.

The watcher is a deposit kind of the watchtowers the Arca wallets already use (`arca watchtower`),
so a seller's wallet deposits where it already deposits its other coins. A maker runs its own.
Anyone can run one.

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
- **The seller's exposure before the payment.** The seller funds `HF` before the buyer commits
  anything. A buyer who walks away leaves the seller's asset in the holding output until `Th`.
