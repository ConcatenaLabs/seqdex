#!/usr/bin/env python3
# Copyright (c) 2026 The Sequentia developers
# Distributed under the MIT software license.
"""The Simplicity order (pkg/covenant SimOrder) built by SeqDEX's Go alone.

An order of the template seqdex/order is one Simplicity program for every
order, its terms in a data leaf beside it. SeqDEX's Go derives the order's
address from the terms and the pinned commitment root, and builds every fill
witness from the template's pinned programs: no compiler runs here. This
proves, against the node's interpreter, with Simplicity active from genesis
and -par=1:

  ADDRESS the address seqob-covenant simderive gives is the script the node
          reads from it, for an order the tapscript FILL leaf cannot carry
          (its fill product is beyond 2^63)
  PASS    that order filled in part at k=0, in part again at k=1 and whole,
          each with the witness seqob-covenant simwitness gives, each judged
          first by seqob-covenant simcheck
  PASS    a Simplicity order at k=0 and a tapscript order at k=1 filled in one
          transaction: SeqDEX takes both leaves side by side
  REJECT  each FORCED INTO A BLOCK, refused for the program's failure, which
          simcheck names first: the credit one atom under the price; the rest
          under other terms (a lower price); the order at input 1 paying at
          outputs 0 and 1
  SIZES   each fill against a tapscript fill in the identical layout (three
          inputs, the fee paid in a third issued asset)

Every fill pays its fee in an issued asset; none touches the policy asset.

Requires the Go toolchain and a configured, built Sequentia node checkout; see
seqob_env.py. seqob-covenant is built once per run.
"""

import json
import os
import subprocess
from decimal import Decimal

import seqob_env  # noqa: F401  (puts the node's test_framework on sys.path)
from test_framework.authproxy import JSONRPCException
from test_framework.test_framework import BitcoinTestFramework, SkipTest
from test_framework.util import assert_equal, BITCOIN_ASSET
from test_framework.address import program_to_witness
from test_framework.key import compute_xonly_pubkey, generate_privkey
from test_framework.messages import (
    COIN, COutPoint, CTransaction, CTxIn, CTxInWitness, CTxOut, CTxOutAsset,
    CTxOutValue, CTxOutWitness, tx_from_hex,
)

import seqob_covenant as cov

FEE = 20_000                      # atoms of Z, the fee asset
INT63 = 2 ** 63 - 1
JET = "Assertion failed inside jet"


def ceil_price(filled, num, den):
    return -(-filled * num // den)


class SeqObSimplicityOrderTest(BitcoinTestFramework):

    def set_test_params(self):
        self.setup_clean_chain = True
        self.num_nodes = 1
        self.extra_args = [[
            "-initialfreecoins=2100000000000000",
            "-anyonecanspendaremine=1",
            "-blindedaddresses=0",
            "-con_default_blinded_addresses=0",
            "-validatepegin=0",
            "-con_parent_chain_signblockscript=51",
            "-con_any_asset_fees=1",
            "-maxtxfee=100.0",
            "-txindex=1",
            "-par=1",
            # Simplicity active from genesis; simplicity:0::: would wait to height 384.
            "-evbparams=simplicity:-1:::",
        ]]

    def skip_test_if_missing_module(self):
        self.skip_if_no_wallet()
        if not os.path.exists(seqob_env.go_bin()):
            raise SkipTest("Go toolchain not found at %s (set GO_BIN)" % seqob_env.go_bin())

    def setup_network(self, split=False):
        self.setup_nodes()

    # --- the Go builder ------------------------------------------------------

    def build_tool(self):
        go = seqob_env.go_bin()
        outdir = os.path.join(self.options.tmpdir, "seqob-bins")
        os.makedirs(outdir, exist_ok=True)
        env = dict(os.environ)
        env["PATH"] = os.path.dirname(go) + os.pathsep + env.get("PATH", "")
        out = os.path.join(outdir, "seqob-covenant")
        subprocess.run([go, "build", "-o", out, "./cmd/seqob-covenant"],
                       cwd=os.path.join(seqob_env.seqdex_dir(), "daemon"), env=env, check=True)
        return out

    def tool(self, args):
        p = subprocess.run([self.tool_bin] + args, capture_output=True, text=True)
        if p.returncode != 0:
            raise AssertionError("seqob-covenant %s: %s" % (args[0], p.stderr))
        return json.loads(p.stdout)

    @staticmethod
    def sim_flags(o):
        return ["-a", o["a"], "-b", o["b"], "-num", str(o["num"]), "-den", str(o["den"]), "-minlot", str(o["minlot"]),
                "-maker", o["maker"].hex(), "-expiry", str(o["expiry"]), "-refund", o["refund"].hex()]

    def simcheck(self, o, tx_hex, k, locked, partial):
        return self.tool(["simcheck"] + self.sim_flags(o) + ["-tx", tx_hex, "-k", str(k), "-locked", str(locked)]
                         + (["-partial"] if partial else []))

    # --- chain helpers -------------------------------------------------------

    def wallet_spk(self):
        node = self.nodes[0]
        a = node.getnewaddress("", "bech32")
        return bytes.fromhex(node.getaddressinfo(node.getaddressinfo(a)["unconfidential"])["scriptPubKey"])

    @staticmethod
    def out(amount, spk, asset_display):
        return CTxOut(nValue=CTxOutValue(amount), scriptPubKey=spk,
                      nAsset=CTxOutAsset(b"\x01" + bytes.fromhex(asset_display)[::-1]))

    def coin(self, spk, atoms, asset):
        """Pay atoms of asset to spk, mine it, and return (txid, vout, atoms)."""
        node = self.nodes[0]
        ver = 0 if spk[0] == 0 else spk[0] - 0x50
        addr = program_to_witness(ver, spk[2:])
        txid = node.sendtoaddress(address=addr, amount=Decimal(atoms) / COIN, assetlabel=asset,
                                  fee_asset_label=BITCOIN_ASSET)
        self.generate(node, 1)
        tx = node.getrawtransaction(txid, True)
        for o in tx["vout"]:
            if o["scriptPubKey"]["hex"] == spk.hex():
                node.lockunspent(False, [{"txid": txid, "vout": o["n"]}])
                return (txid, o["n"], atoms)
        raise AssertionError("no output to %s" % spk.hex())

    def mktx(self, ins, outs):
        tx = CTransaction()
        tx.nVersion = 2
        for (txid, vout, _) in ins:
            tx.vin.append(CTxIn(COutPoint(int(txid, 16), vout), nSequence=0xffffffff))
        tx.vout = list(outs)
        tx.wit.vtxinwit = [CTxInWitness() for _ in tx.vin]
        tx.wit.vtxoutwit = [CTxOutWitness() for _ in tx.vout]
        return tx

    def finish(self, tx, stacks):
        """The wallet signs its coins; each order input takes its stack."""
        for k in stacks:
            tx.wit.vtxinwit[k].scriptWitness.stack = []
        signed = self.nodes[0].signrawtransactionwithwallet(tx.serialize().hex())
        t2 = tx_from_hex(signed["hex"])
        while len(t2.wit.vtxinwit) < len(t2.vin):
            t2.wit.vtxinwit.append(CTxInWitness())
        for k, st in stacks.items():
            t2.wit.vtxinwit[k].scriptWitness.stack = [bytes.fromhex(x) if isinstance(x, str) else x for x in st]
        return t2.serialize().hex()

    def forced_block(self, tx_hex):
        node = self.nodes[0]
        h0 = node.getblockcount()
        try:
            node.generateblock(node.getnewaddress(), [tx_hex], invalid_call=False)
        except JSONRPCException as e:
            assert_equal(node.getblockcount(), h0)
            return e.error["message"]
        assert_equal(node.getblockcount(), h0 + 1)
        return None

    def mine(self, label, tx_hex):
        node = self.nodes[0]
        r = node.testmempoolaccept([tx_hex])[0]
        assert r["allowed"], (label, r)
        txid = node.sendrawtransaction(tx_hex)
        self.generate(node, 1)
        assert node.getrawtransaction(txid, True)["confirmations"] >= 1
        d = node.decoderawtransaction(tx_hex)
        self.sizes[label] = {"vsize": d["vsize"], "weight": d["weight"]}
        self.log.info("PASS %-48s %s vsize=%d", label, txid, d["vsize"])
        return txid

    def refuse(self, label, tx_hex, expect):
        node = self.nodes[0]
        r = node.testmempoolaccept([tx_hex])[0]
        assert not r["allowed"], "%s: the mempool accepts it" % label
        err = self.forced_block(tx_hex)
        assert err is not None, "%s: MINED" % label
        assert expect in err, (label, err)
        self.log.info("REJECT %-46s block: %s", label, err)
        return err

    # --- the test ------------------------------------------------------------

    def run_test(self):
        node = self.nodes[0]
        self.tool_bin = self.build_tool()
        self.sizes = {}
        self.generate(node, 101)
        node.sendtoaddress(address=node.getnewaddress(), amount=1000000, fee_asset_label=BITCOIN_ASSET)
        self.generate(node, 1)
        X = node.issueasset(assetamount=1000000, tokenamount=0, blind=False, fee_asset=BITCOIN_ASSET)["asset"]
        Y = node.issueasset(assetamount=1000000, tokenamount=0, blind=False, fee_asset=BITCOIN_ASSET)["asset"]
        Z = node.issueasset(assetamount=1000000, tokenamount=0, blind=False, fee_asset=BITCOIN_ASSET)["asset"]
        self.generate(node, 1)
        rates = node.getfeeexchangerates()
        rates.update({Y: 100000000, Z: 100000000})
        node.setfeeexchangerates(rates)
        internal = lambda d: bytes.fromhex(d)[::-1].hex()

        maker_spk = self.wallet_spk()
        taker_spk, change_spk = self.wallet_spk(), self.wallet_spk()
        refund = compute_xonly_pubkey(generate_privkey())[0]
        LOCKED = 20_000_000_000_000
        S = {"a": internal(X), "b": internal(Y), "num": 9_000_000_000_007, "den": 10_000_000_000_003,
             "minlot": 1_000_000, "maker": maker_spk, "expiry": node.getblockcount() + 1000, "refund": refund}
        assert LOCKED * S["num"] + S["den"] - 1 > INT63, "the tapscript leaf could carry this order"
        d = self.tool(["simderive"] + self.sim_flags(S))
        spk = bytes.fromhex(d["scriptpubkey"])
        info = node.getaddressinfo(d["address"]["elementsregtest"])
        assert_equal(info["scriptPubKey"], d["scriptpubkey"])
        self.log.info("ADDRESS %s from the terms and root %s (template %s), the node reads the same script",
                      d["address"]["elementsregtest"], d["cmr"], d["template_hash"])
        s0 = self.coin(spk, LOCKED, X)

        # A tapscript order of the same shape, inside its 2^63 bound, for sizes.
        mk = compute_xonly_pubkey(generate_privkey())[0]
        T = dict(a=internal(X), b=internal(Y), num=9, den=10, minlot=1_000_000, prog=mk,
                 expiry=node.getblockcount() + 1000)
        tap, fill_leaf, refund_leaf = cov.order_taptree(bytes.fromhex(T["a"]), bytes.fromhex(T["b"]), T["num"], T["den"],
                                                        mk, T["minlot"], T["expiry"], mk)
        t_spk = bytes(tap.scriptPubKey)
        t_credit_spk = bytes([0x51, 0x20]) + mk
        t0 = self.coin(t_spk, LOCKED, X)
        t_witness = cov.fill_witness(tap, fill_leaf)

        pays = [self.coin(self.wallet_spk(), 30_000_000_000_000, Y) for _ in range(3)]
        fees = [self.coin(self.wallet_spk(), 1_000_000, Z) for _ in range(8)]

        def layout(order_coin, credit, rest, filled, pay, fee, k=0, gap=None):
            """The order at input k (the pay coin before it for k=1): credit
            at 2k, rest (or for a whole fill the taker's own receipt) at 2k+1."""
            paid = credit.nValue.getAmount()
            mine = [self.out(filled, taker_spk, X) if rest is not None else None,
                    self.out(pay[2] - paid, change_spk, Y)]
            pair = [credit, rest if rest is not None else (gap or self.out(filled, taker_spk, X))]
            tail = [self.out(fee[2] - FEE, change_spk, Z), CTxOut(CTxOutValue(FEE), b"",
                                                                   CTxOutAsset(b"\x01" + bytes.fromhex(Z)[::-1]))]
            mine = [m for m in mine if m is not None]
            if k == 0:
                return self.mktx([order_coin, pay, fee], pair + mine + tail)
            return self.mktx([pay, order_coin, fee], mine + pair + tail)

        # ---- the Simplicity order, partial at input 0 -------------------------
        F1 = 6_000_000_000_001
        p1 = ceil_price(F1, S["num"], S["den"])
        stack_p = self.tool(["simwitness"] + self.sim_flags(S) + ["-path", "fill"])["stack"]
        assert_equal(stack_p[0], d["terms"] + "40")

        def s_partial(credit_value=p1, rest_spk=spk, k=0, coin=s0, filled=F1):
            tx = layout(coin, self.out(credit_value, maker_spk, Y), self.out(coin[2] - filled, rest_spk, X), filled,
                        pays[0], fees[0], k)
            return self.finish(tx, {k: stack_p})

        good = s_partial()
        c = self.simcheck(S, good, 0, LOCKED, True)
        assert c["ok"] and c["filled"] == F1 and c["required"] == p1, c
        # Negatives, each judged by Go first and then refused in a block.
        under = s_partial(credit_value=p1 - 1)
        c = self.simcheck(S, under, 0, LOCKED, True)
        assert not c["ok"] and "below the price" in c["error"], c
        self.refuse("credit one atom under the price", under, JET)
        lower = dict(S, num=S["num"] - 1)
        lower_spk = bytes.fromhex(self.tool(["simderive"] + self.sim_flags(lower))["scriptpubkey"])
        other_terms = s_partial(rest_spk=lower_spk)
        c = self.simcheck(S, other_terms, 0, LOCKED, True)
        assert not c["ok"] and "own script" in c["error"], c
        self.refuse("rest under other terms (a lower price)", other_terms, JET)
        # The order at input 1 with its credit and rest at outputs 0 and 1.
        wrong = layout(s0, self.out(p1, maker_spk, Y), self.out(LOCKED - F1, spk, X), F1, pays[0], fees[0], k=0)
        wrong.vin[0], wrong.vin[1] = wrong.vin[1], wrong.vin[0]
        wrong = self.finish(wrong, {1: stack_p})
        c = self.simcheck(S, wrong, 1, LOCKED, True)
        assert not c["ok"], c
        self.refuse("order at input 1 paying at outputs 0 and 1", wrong, JET)
        txid1 = self.mine("simplicity partial fill, k=0", good)

        # The tapscript order, partial at input 0, in the identical layout.
        tp = ceil_price(F1, T["num"], T["den"])
        ttx = layout(t0, self.out(tp, t_credit_spk, Y), self.out(LOCKED - F1, t_spk, X), F1, pays[1], fees[1])
        ttxid = self.mine("tapscript partial fill, k=0", self.finish(ttx, {0: t_witness}))

        # ---- the Simplicity order, partial again at input 1 ---------------------
        s1 = (txid1, 1, LOCKED - F1)
        F2 = 5_000_000_000_000
        pay2 = (txid1, 3, pays[0][2] - p1)
        node.lockunspent(False, [{"txid": txid1, "vout": 3}])
        k1 = layout(s1, self.out(ceil_price(F2, S["num"], S["den"]), maker_spk, Y), self.out(s1[2] - F2, spk, X),
                    F2, pay2, fees[2], k=1)
        k1 = self.finish(k1, {1: stack_p})
        c = self.simcheck(S, k1, 1, s1[2], True)
        assert c["ok"] and (c["credit_index"], c["remainder_index"]) == (2, 3), c
        txid2 = self.mine("simplicity partial fill, k=1", k1)

        # ---- whole: output 1 is the taker's X ----------------------------------
        s2 = (txid2, 3, s1[2] - F2)
        pay3 = (txid2, 1, pay2[2] - ceil_price(F2, S["num"], S["den"]))
        node.lockunspent(False, [{"txid": txid2, "vout": 1}])
        stack_w = self.tool(["simwitness"] + self.sim_flags(S) + ["-path", "whole"])["stack"]
        assert_equal(stack_w[0], d["terms"] + "00")
        w = layout(s2, self.out(ceil_price(s2[2], S["num"], S["den"]), maker_spk, Y), None, s2[2], pay3, fees[3])
        w = self.finish(w, {0: stack_w})
        c = self.simcheck(S, w, 0, s2[2], False)
        assert c["ok"] and c["filled"] == s2[2], c
        self.mine("simplicity whole fill, k=0", w)
        # The tapscript whole fill needs an output in another asset at 2k+1.
        t1 = (ttxid, 1, LOCKED - F1)
        tpay = (ttxid, 3, pays[1][2] - tp)
        node.lockunspent(False, [{"txid": ttxid, "vout": 3}])
        tw = layout(t1, self.out(ceil_price(t1[2], T["num"], T["den"]), t_credit_spk, Y), None, t1[2], tpay, fees[4],
                    gap=self.out(tpay[2] - ceil_price(t1[2], T["num"], T["den"]), change_spk, Y))
        # The gap is the Y change; the taker's X follows it.
        tw.vout[2] = self.out(t1[2], taker_spk, X)
        tw.wit.vtxoutwit = [CTxOutWitness() for _ in tw.vout]
        self.mine("tapscript whole fill, k=0", self.finish(tw, {0: t_witness}))

        # ---- both leaves in one transaction: Simplicity at 0, tapscript at 1 ---
        E = dict(S, num=1, den=1, minlot=1, expiry=S["expiry"] + 1)
        e_spk = bytes.fromhex(self.tool(["simderive"] + self.sim_flags(E))["scriptpubkey"])
        e0 = self.coin(e_spk, 3_000_000, X)
        t2 = self.coin(t_spk, 2_000_000, X)
        pay4 = pays[2]
        fee = fees[5]
        e_paid, t_paid = 1_000_000, ceil_price(2_000_000, T["num"], T["den"])
        both = self.mktx([e0, t2, pay4, fee], [
            self.out(e_paid, maker_spk, Y), self.out(2_000_000, e_spk, X),          # E: credit, rest
            self.out(t_paid, t_credit_spk, Y), self.out(pay4[2] - e_paid - t_paid, change_spk, Y),  # T: credit, gap
            self.out(1_000_000 + 2_000_000, taker_spk, X),
            self.out(fee[2] - FEE, change_spk, Z),
            CTxOut(CTxOutValue(FEE), b"", CTxOutAsset(b"\x01" + bytes.fromhex(Z)[::-1]))])
        stack_e = self.tool(["simwitness"] + self.sim_flags(E) + ["-path", "fill"])["stack"]
        # The 1:1 order's program is the same bytes as every other order's.
        assert_equal(stack_e[1], stack_p[1])
        both = self.finish(both, {0: stack_e, 1: t_witness})
        c = self.simcheck(E, both, 0, 3_000_000, True)
        assert c["ok"], c
        self.mine("simplicity at k=0 and tapscript at k=1", both)

        for k, v in self.sizes.items():
            self.log.info("SIZE %-46s %5d vB  %5d WU", k, v["vsize"], v["weight"])
        out = os.environ.get("SEQOB_SIM_SIZES")
        if out:
            with open(out, "w") as f:
                json.dump(self.sizes, f, indent=1)


if __name__ == "__main__":
    SeqObSimplicityOrderTest().main()
