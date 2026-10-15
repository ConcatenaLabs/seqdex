#!/usr/bin/env python3
# Copyright (c) 2026 The Sequentia developers
# Distributed under the MIT software license.
"""SeqOB covenant FILL/REFUND leaves: passive, unilaterally-fillable resting orders.

A maker locks N units of explicit asset A in ONE taproot UTXO ("the order is the
coin"). The taproot output has internal key = NUMS (no key-path spend) and a two
leaf tree {FILL, REFUND}:

  FILL   (permissionless, NO maker signature) -- anyone may spend the order iff
         the spending transaction pays the maker the agreed price of asset B.
         Every order parameter (asset A id, asset B id, rate_num/rate_den, the
         maker payout scriptPubKey, min_lot) is a compile-time constant baked
         into the leaf, so it is committed inside the taproot output key: a taker
         can only satisfy the terms, never alter them. The leaf reads the price
         it must be paid entirely from transaction introspection; it needs NO
         witness data at all (witness = [FILL_script, control_block]).

  REFUND <expiry> OP_CHECKLOCKTIMEVERIFY OP_DROP <P_maker> OP_CHECKSIG -- after
         the absolute-locktime expiry the maker reclaims the order.

Design points enforced by the FILL leaf (see
docs/simplicity-dex-covenant-offers-design.md, sections 1-2, and
section 6 of DESIGN.md in the openamp repository):

  * Explicit-only. Every introspected asset/value prefix must be 0x01. A blinded
    (confidential) credit output the covenant cannot read is rejected.
  * Input-bound output map (anti-aliasing). The covenant input at consensus index
    k credits the maker at output 2k and re-pays its remainder at output 2k+1.
    Because the index is derived per input from OP_PUSHCURRENTINPUTINDEX, two
    covenant inputs can never both reference one shared maker-credit output, so a
    single payment can never settle two orders.
  * filled = locked - remainder, where `locked` is this input's own value
    (OP_INSPECTINPUTVALUE) and `remainder` is asset A re-paid to output 2k+1.
    For a FULL fill the taker supplies no asset-A output at 2k+1 (remainder 0).
  * Self-replication for partial fills is the last_will pattern: the remainder
    output's scriptPubKey must EQUAL this input's own scriptPubKey (compared via
    OP_INSPECTINPUTSCRIPTPUBKEY vs OP_INSPECTOUTPUTSCRIPTPUBKEY). Because the same
    order re-uses the identical covenant, direct scriptPubKey equality is
    sufficient and needs no OP_TWEAKVERIFY / tagged-hash self-proof (that
    machinery is only needed when the OUTPUT covenant differs from the INPUT
    covenant, e.g. per-holder keys in a self-referential covenant).
  * The maker must be paid at least ceil(filled * rate_num / rate_den) of asset B
    (ceil rounds in the maker's favour), computed with OP_MUL64/OP_ADD64/OP_DIV64
    as floor((filled*num + den - 1) / den).
  * Dust-griefing floors: filled >= min_lot, and any remainder >= min_lot.

Overflow bound (documented, asserted by the caller): OP_MUL64/OP_ADD64 abort on
signed-64-bit overflow, so orders must satisfy rate_num * N + rate_den - 1 < 2**63
(N = locked atoms). A wrong bound only self-harms the maker; pick sizes/rates
below it (u256 jets would lift it, unnecessary here).
"""

import seqob_env  # noqa: F401  (puts the node's test_framework on sys.path)
from test_framework.script import (
    CScript, taproot_construct,
    OP_1, OP_1ADD, OP_ADD, OP_CHECKLOCKTIMEVERIFY, OP_CHECKSIG, OP_DROP, OP_DUP,
    OP_ELSE, OP_ENDIF, OP_EQUAL, OP_EQUALVERIFY, OP_IF, OP_LESSTHAN, OP_NIP,
    OP_ROT, OP_SWAP, OP_VERIFY,
    OP_INSPECTINPUTVALUE, OP_INSPECTINPUTSCRIPTPUBKEY, OP_PUSHCURRENTINPUTINDEX,
    OP_INSPECTOUTPUTASSET, OP_INSPECTOUTPUTVALUE, OP_INSPECTOUTPUTSCRIPTPUBKEY,
    OP_INSPECTNUMOUTPUTS,
    OP_ADD64, OP_SUB64, OP_MUL64, OP_DIV64, OP_GREATERTHANOREQUAL64,
)

# BIP341 nothing-up-my-sleeve point: taproot internal key with no known discrete
# log, so a NUMS-internal-key output has no key-path spend.
NUMS = bytes.fromhex("50929b74c1a04954b78b4b6035e97a5e078a5a0f28ec96d547bfee9ace803ac0")


def le8(n):
    """A 64-bit little-endian push constant, the on-stack form of OP_*64 operands."""
    assert 0 <= n < (1 << 63)
    return int(n).to_bytes(8, "little")


# Index-map helpers. The covenant input at consensus index k must credit the
# maker at output 2k and re-pay its remainder at output 2k+1. These recompute the
# indices from OP_PUSHCURRENTINPUTINDEX each time (a single push, cheaper than
# stack juggling), so the leaf carries no per-spend index state.
_CREDIT_IDX = [OP_PUSHCURRENTINPUTINDEX, OP_DUP, OP_ADD]           # 2k
_REM_IDX = [OP_PUSHCURRENTINPUTINDEX, OP_DUP, OP_ADD, OP_1ADD]     # 2k + 1


def build_fill_leaf(asset_a, asset_b, rate_num, rate_den, maker_prog, min_lot,
                    maker_ver=1):
    """The permissionless FILL leaf for one resting order.

    asset_a, asset_b : 32-byte internal-order asset ids (the resting asset and
                       the payment asset).
    rate_num/rate_den: price. required_B = ceil(filled * rate_num / rate_den).
    maker_prog       : the maker credit output's witness program (32 bytes for a
                       v1 taproot payout).
    maker_ver        : the maker credit output's witness version (default 1).
    min_lot          : dust-griefing floor (atoms), on both `filled` and any
                       remainder.
    """
    assert len(asset_a) == 32 and len(asset_b) == 32
    assert rate_num >= 1 and rate_den >= 1 and min_lot >= 1
    assert maker_ver == 1, "this builder pins a v1 taproot maker payout"

    s = []
    # ---- locked = this covenant input's own value (must be explicit) ----
    s += [OP_PUSHCURRENTINPUTINDEX, OP_INSPECTINPUTVALUE]   # [locked8, prefix]
    s += [OP_1, OP_EQUALVERIFY]                             # [locked8]

    # ---- remainder = asset A re-paid to output 2k+1 (0 for a full fill) ----
    s += _REM_IDX + [OP_INSPECTNUMOUTPUTS, OP_LESSTHAN]     # [locked8, (2k+1 < numouts)]
    s += [OP_IF]
    #   output 2k+1 exists: is it asset A (explicit)?
    s += _REM_IDX + [OP_INSPECTOUTPUTASSET]                 # [locked8, asset32, prefix]
    s += [OP_1, OP_EQUALVERIFY]                             # [locked8, asset32]   explicit
    s += [asset_a, OP_EQUAL]                                # [locked8, isA]
    s += [OP_IF]
    #     asset A at 2k+1 -> it is the remainder: self-replicate + floor + value
    s += _REM_IDX + [OP_INSPECTOUTPUTSCRIPTPUBKEY]          # [locked8, outprog, outver]
    s += [OP_PUSHCURRENTINPUTINDEX, OP_INSPECTINPUTSCRIPTPUBKEY]  # [.., outprog, outver, inprog, inver]
    s += [OP_ROT, OP_EQUALVERIFY, OP_EQUALVERIFY]           # outver==inver, outprog==inprog -> [locked8]
    s += _REM_IDX + [OP_INSPECTOUTPUTVALUE, OP_1, OP_EQUALVERIFY]  # [locked8, remainder8]
    s += [OP_DUP, le8(min_lot), OP_GREATERTHANOREQUAL64, OP_VERIFY]  # remainder >= min_lot
    s += [OP_ELSE]
    #     not asset A (taker change / receipt): remainder = 0
    s += [le8(0)]                                           # [locked8, 0]
    s += [OP_ENDIF]
    s += [OP_ELSE]
    #   output 2k+1 absent: full fill, remainder = 0
    s += [le8(0)]                                           # [locked8, 0]
    s += [OP_ENDIF]

    # ---- filled = locked - remainder, floored by min_lot ----
    s += [OP_SUB64, OP_VERIFY]                              # [filled8]
    s += [OP_DUP, le8(min_lot), OP_GREATERTHANOREQUAL64, OP_VERIFY]  # filled >= min_lot

    # ---- required_B = ceil(filled * num / den) = floor((filled*num + den-1)/den) ----
    s += [le8(rate_num), OP_MUL64, OP_VERIFY]               # [p8]           p = filled*num
    s += [le8(rate_den - 1), OP_ADD64, OP_VERIFY]           # [(p+den-1)8]
    s += [le8(rate_den), OP_DIV64, OP_VERIFY, OP_NIP]       # DIV64 -> [r,q,1]; VERIFY; NIP -> [required8]

    # ---- credit output at 2k: asset == B, spk == maker, value >= required ----
    s += _CREDIT_IDX + [OP_INSPECTOUTPUTASSET, OP_1, OP_EQUALVERIFY, asset_b, OP_EQUALVERIFY]
    s += _CREDIT_IDX + [OP_INSPECTOUTPUTSCRIPTPUBKEY, OP_1, OP_EQUALVERIFY, maker_prog, OP_EQUALVERIFY]
    s += _CREDIT_IDX + [OP_INSPECTOUTPUTVALUE, OP_1, OP_EQUALVERIFY]  # [required8, cvalue8]
    s += [OP_SWAP, OP_GREATERTHANOREQUAL64]                 # cvalue >= required  (sole result)
    return CScript(s)


def build_refund_leaf(expiry_locktime, maker_x):
    """REFUND: absolute-CLTV reclaim by the maker after expiry."""
    assert len(maker_x) == 32
    return CScript([expiry_locktime, OP_CHECKLOCKTIMEVERIFY, OP_DROP, maker_x, OP_CHECKSIG])


def order_taptree(asset_a, asset_b, rate_num, rate_den, maker_prog, min_lot,
                  expiry_locktime, maker_x, internal_key=NUMS):
    """Build the {FILL, REFUND} taproot order. internal_key defaults to NUMS
    (no key-path spend); a maker who wants an instant key-path cancel may pass
    P_maker instead, in which case a taker MUST treat a non-NUMS internal key as
    a maker-cancellable offer (client-side verify)."""
    fill = build_fill_leaf(asset_a, asset_b, rate_num, rate_den, maker_prog, min_lot)
    refund = build_refund_leaf(expiry_locktime, maker_x)
    tap = taproot_construct(internal_key, [("fill", fill), ("refund", refund)])
    return tap, fill, refund


def control_block(tap, leaf_name):
    """The taproot control block for a script-path spend of the named leaf."""
    leaf = tap.leaves[leaf_name]
    return bytes([leaf.version + tap.negflag]) + tap.internal_pubkey + leaf.merklebranch


def fill_witness(tap, fill):
    """FILL is fully introspection-driven: the covenant reads the price it must be
    paid from the transaction, so there is no data to supply. Witness stack is
    just the leaf script and its control block."""
    return [bytes(fill), control_block(tap, "fill")]


def refund_witness(tap, refund, sig_maker):
    """REFUND spends with the maker's schnorr signature over the refund leaf."""
    return [sig_maker, bytes(refund), control_block(tap, "refund")]
