#!/usr/bin/env python3
# Copyright (c) 2026 The Sequentia developers
# Distributed under the MIT software license.
"""Several covenant orders filled in ONE transaction, order k at input k.

Each order's FILL leaf reads only the outputs bound to its own input index k:
the maker's credit at 2k and, at 2k+1, the order resting again under its
identical script (a partial fill) or an explicit output in any other asset (a
full fill's "gap"). So a transaction may fill several orders at once, as a
taker taking two books does, or an operator's round filling every order its
takers asked for: the orders take inputs 0..n-1 and outputs 0..2n-1.

The transaction here is laid out by SeqDEX's own planner (seqob-covenant
filltx, i.e. pkg/covenant BuildFillTx) and judged by its reading of the leaf
(seqob-covenant check, pkg/covenant CheckFill). This proves both against the
script interpreter:

  PASS   order 1 filled in part at k=0 (credit 0, rest 1) and order 2 filled
         whole at k=1 (credit 2, gap 3), in one block; order 2's REFUND lock is
         a median time, order 1's a height
  REJECT each wrong layout, FORCED INTO A BLOCK with generateblock (relay
         policy cannot hide a consensus flaw), refused for the script failure
         CheckFill names, with -par=1 so the block error carries the reason:
           credits swapped between 0 and 2
           order 2's credit one atom short
           order 2's gap holds the asset the order sells
           order 1's rest paid to another script
           order 1's rest below min_lot
  VECTORS the planner's vectors (pkg/covenant/testdata/fill_vectors.json, which
         the web wallet's covenant.js reproduces byte for byte) decode on the
         node as the transactions they claim to be

Not run here: a confidential output at 2k or 2k+1 (CheckFill refuses one; a
real blinded output needs a blinding round this test does not do).

Requires the Go toolchain and a configured, built Sequentia node checkout; see
seqob_env.py. seqob-covenant is built once per run.
"""

import json
import os
import subprocess
import time
from decimal import Decimal

import seqob_env  # noqa: F401  (puts the node's test_framework on sys.path)
from test_framework.authproxy import JSONRPCException
from test_framework.test_framework import BitcoinTestFramework, SkipTest
from test_framework.util import assert_equal, satoshi_round, BITCOIN_ASSET
from test_framework.key import compute_xonly_pubkey, generate_privkey
from test_framework.messages import (
    COIN, COutPoint, CTransaction, CTxIn, CTxInWitness, CTxOut, CTxOutAsset,
    CTxOutValue, tx_from_hex,
)
from test_framework.script import CScript, OP_1

import seqob_covenant as cov

FEE = 5000
MIN_LOT = 1 * COIN


def go_bin():
    return seqob_env.go_bin()


def seqdex_dir():
    return seqob_env.seqdex_dir()


class SeqObMultiFillTest(BitcoinTestFramework):

    def set_test_params(self):
        self.setup_clean_chain = True
        self.num_nodes = 1
        self.extra_args = [[
            "-initialfreecoins=2100000000000000",
            "-anyonecanspendaremine=1",
            "-blindedaddresses=0",
            "-validatepegin=0",
            "-con_parent_chain_signblockscript=51",
            "-con_any_asset_fees=1",
            "-maxtxfee=100.0",
            "-txindex=1",
            "-par=1",
        ]]

    def skip_test_if_missing_module(self):
        self.skip_if_no_wallet()
        if not os.path.exists(go_bin()):
            raise SkipTest("Go toolchain not found at %s (set GO_BIN)" % go_bin())
        if not os.path.isdir(os.path.join(seqdex_dir(), "daemon")):
            raise SkipTest("seqdex daemon dir not found at %s (set SEQDEX_DIR)"
                           % os.path.join(seqdex_dir(), "daemon"))

    def setup_network(self, split=False):
        self.setup_nodes()

    # --- the Go planner -----------------------------------------------------

    def build_tool(self):
        go = go_bin()
        outdir = os.path.join(self.options.tmpdir, "seqob-bins")
        os.makedirs(outdir, exist_ok=True)
        env = dict(os.environ)
        env["PATH"] = os.path.dirname(go) + os.pathsep + env.get("PATH", "")
        out = os.path.join(outdir, "seqob-covenant")
        self.log.info("building seqob-covenant ...")
        subprocess.run([go, "build", "-o", out, "./cmd/seqob-covenant"],
                       cwd=os.path.join(seqdex_dir(), "daemon"), env=env, check=True)
        return out

    def tool(self, args, stdin=None):
        p = subprocess.run([self.tool_bin] + args, input=stdin, capture_output=True, text=True)
        if p.returncode != 0:
            raise AssertionError("seqob-covenant %s: %s" % (args[0], p.stderr))
        return json.loads(p.stdout)

    def order_flags(self, o):
        return ["-a", o["asset_a"], "-b", o["asset_b"], "-num", str(o["rate_num"]), "-den", str(o["rate_den"]),
                "-minlot", str(o["min_lot"]), "-prog", o["maker_prog"], "-expiry", str(o["expiry_locktime"]),
                "-makerx", o["maker_x"]]

    def check(self, terms, tx_hex, k, locked):
        return self.tool(["check"] + self.order_flags(terms) + ["-tx", tx_hex, "-k", str(k), "-locked", str(locked)])

    # --- on-chain helpers ---------------------------------------------------

    def wallet_spk(self):
        addr = self.nodes[0].getnewaddress()
        unconf = self.nodes[0].getaddressinfo(addr)["unconfidential"]
        return bytes.fromhex(self.nodes[0].getaddressinfo(unconf)["scriptPubKey"])

    def ctxout(self, amount, spk, asset_display):
        return CTxOut(nValue=CTxOutValue(amount), scriptPubKey=spk,
                      nAsset=CTxOutAsset(b"\x01" + bytes.fromhex(asset_display)[::-1]))

    def fresh_utxo(self, amount, asset_display):
        node = self.nodes[0]
        unconf = node.getaddressinfo(node.getnewaddress("", "bech32"))["unconfidential"]
        if asset_display == BITCOIN_ASSET:
            node.sendtoaddress(address=unconf, amount=amount, fee_asset_label=BITCOIN_ASSET)
        else:
            node.sendtoaddress(address=unconf, amount=amount, assetlabel=asset_display, fee_asset_label=BITCOIN_ASSET)
        self.generate(node, 1)
        for u in node.listunspent():
            if (u["asset"] == asset_display and abs(float(u["amount"]) - amount) < 1e-9
                    and u["scriptPubKey"].startswith("0014") and u["spendable"]):
                return u
        raise AssertionError("no fresh utxo of %s" % asset_display)

    def fund_order(self, spk_hex, asset_display, units):
        """Rest `units` of asset_display at the order's script, as a maker does."""
        node = self.nodes[0]
        a = self.fresh_utxo(units + 1, asset_display)
        btc = self.fresh_utxo(1, BITCOIN_ASSET)
        tx = CTransaction()
        tx.nVersion = 2
        tx.vin.append(CTxIn(COutPoint(int(a["txid"], 16), a["vout"])))
        tx.vin.append(CTxIn(COutPoint(int(btc["txid"], 16), btc["vout"])))
        tx.vout.append(self.ctxout(units * COIN, bytes.fromhex(spk_hex), asset_display))
        tx.vout.append(self.ctxout(COIN, self.wallet_spk(), asset_display))
        tx.vout.append(self.ctxout(int(satoshi_round(btc["amount"]) * COIN) - FEE, self.wallet_spk(), BITCOIN_ASSET))
        tx.vout.append(CTxOut(CTxOutValue(FEE)))
        signed = node.signrawtransactionwithwallet(tx.serialize().hex())
        assert signed["complete"], signed
        txid = node.sendrawtransaction(signed["hex"])
        self.generate(node, 1)
        return txid, 0

    def sign(self, unsigned_hex, witnesses):
        """Sign the taker's inputs, then put each order's FILL witness (the
        planner's [leaf, control block]) on its input: the settler's
        sign-then-inject order."""
        tx = tx_from_hex(unsigned_hex)
        n_orders = len(witnesses)
        for k in range(n_orders):
            tx.wit.vtxinwit[k].scriptWitness.stack = []
        signed = self.nodes[0].signrawtransactionwithwallet(tx.serialize().hex())
        tx = tx_from_hex(signed["hex"])
        while len(tx.wit.vtxinwit) < len(tx.vin):
            tx.wit.vtxinwit.append(CTxInWitness())
        for k in range(n_orders):
            tx.wit.vtxinwit[k].scriptWitness.stack = witnesses[k]
        return tx.serialize().hex()

    def forced_block(self, tx_hex):
        """Force tx_hex into a block, past the mempool. Returns the block error, or None if mined."""
        node = self.nodes[0]
        h0 = node.getblockcount()
        try:
            node.generateblock(node.getnewaddress(), [tx_hex], invalid_call=False)
        except JSONRPCException as e:
            assert_equal(node.getblockcount(), h0)
            return str(e)
        assert_equal(node.getblockcount(), h0 + 1)
        return None

    # --- the test -----------------------------------------------------------

    def run_test(self):
        node = self.nodes[0]
        self.tool_bin = self.build_tool()
        self.generate(node, 101)
        node.sendtoaddress(address=node.getnewaddress(), amount=1000000, fee_asset_label=BITCOIN_ASSET)
        self.generate(node, 1)

        self.check_vectors()

        X = node.issueasset(assetamount=100000, tokenamount=0, blind=False, fee_asset=BITCOIN_ASSET)["asset"]
        Y = node.issueasset(assetamount=100000, tokenamount=0, blind=False, fee_asset=BITCOIN_ASSET)["asset"]
        Z = node.issueasset(assetamount=100000, tokenamount=0, blind=False, fee_asset=BITCOIN_ASSET)["asset"]
        self.generate(node, 1)
        internal = lambda d: bytes.fromhex(d)[::-1].hex()

        maker1 = compute_xonly_pubkey(generate_privkey())[0]
        maker2 = compute_xonly_pubkey(generate_privkey())[0]
        mtp = node.getblockheader(node.getbestblockhash())["mediantime"]
        # Order 1 sells 60 X at 2 Y each, refundable from a height; order 2 sells
        # 40 X at 2.5 Z each, refundable from a median time a day ahead.
        o1 = {"asset_a": internal(X), "asset_b": internal(Y), "rate_num": 2, "rate_den": 1, "min_lot": MIN_LOT,
              "maker_prog": maker1.hex(), "maker_prog_ver": 1, "expiry_locktime": node.getblockcount() + 500,
              "maker_x": maker1.hex()}
        o2 = {"asset_a": internal(X), "asset_b": internal(Z), "rate_num": 5, "rate_den": 2, "min_lot": MIN_LOT,
              "maker_prog": maker2.hex(), "maker_prog_ver": 1, "expiry_locktime": mtp + 86400,
              "maker_x": maker2.hex()}
        assert o2["expiry_locktime"] >= 500000000
        spk1 = self.tool(["derive"] + self.order_flags(o1))["scriptpubkey"]
        spk2 = self.tool(["derive"] + self.order_flags(o2))["scriptpubkey"]
        # The Go builder's scripts are the proven Python builder's.
        tap1, _, _ = cov.order_taptree(bytes.fromhex(o1["asset_a"]), bytes.fromhex(o1["asset_b"]), 2, 1, maker1,
                                       MIN_LOT, o1["expiry_locktime"], maker1)
        assert_equal(spk1, bytes(tap1.scriptPubKey).hex())
        at1 = self.fund_order(spk1, X, 60)
        at2 = self.fund_order(spk2, X, 40)
        l1, f1, l2, f2 = 60 * COIN, 40 * COIN, 40 * COIN, 40 * COIN
        self.log.info("order 1 rests at %s:%d (height lock), order 2 at %s:%d (median-time lock %d)",
                      at1[0], at1[1], at2[0], at2[1], o2["expiry_locktime"])

        y = self.fresh_utxo(100, Y)
        z = self.fresh_utxo(150, Z)
        btc = self.fresh_utxo(1, BITCOIN_ASSET)
        taker = self.wallet_spk().hex()
        spec = {
            "takes": [
                {"terms": o1, "txid": at1[0], "vout": at1[1], "locked": l1, "filled": f1},
                {"terms": o2, "txid": at2[0], "vout": at2[1], "locked": l2, "filled": f2},
            ],
            # Order 2 is filled whole: the taker's Z change stands at its 2k+1.
            "gaps": [None, {"asset": internal(Z), "value": 50 * COIN, "script": taker}],
            "inputs": [{"txid": u["txid"], "vout": u["vout"]} for u in (y, z, btc)],
            "tail": [
                {"asset": internal(X), "value": f1 + f2, "script": taker},              # the taker's X
                {"asset": internal(Y), "value": 20 * COIN, "script": taker},            # Y change
                {"asset": internal(BITCOIN_ASSET), "value": COIN - FEE, "script": taker},
                {"asset": internal(BITCOIN_ASSET), "value": FEE, "script": ""},         # the fee
            ],
        }
        built = self.tool(["filltx"], stdin=json.dumps(spec))
        for k, p in enumerate(built["plans"]):
            self.log.info("  planner: order %d at input %d, credit %d, rest/gap %d, required %d, remainder %d",
                          k + 1, p["input_index"], p["credit_index"], p["remainder_index"], p["required_b"], p["remainder"])
            assert_equal((p["input_index"], p["credit_index"], p["remainder_index"]), (k, 2 * k, 2 * k + 1))
        planned = tx_from_hex(built["hex"])
        fill_witnesses = [list(planned.wit.vtxinwit[k].scriptWitness.stack) for k in range(2)]
        assert all(len(w) == 2 for w in fill_witnesses)
        good = self.sign(built["hex"], fill_witnesses)
        for k, (terms, locked) in enumerate(((o1, l1), (o2, l2))):
            c = self.check(terms, good, k, locked)
            assert c["ok"], c

        def mutate(change):
            tx = tx_from_hex(good)
            change(tx.vout)
            for k in range(2):
                tx.wit.vtxinwit[k].scriptWitness.stack = []
            return self.sign(tx.serialize().hex(), fill_witnesses)

        def swap(a, b):
            def f(v):
                v[a], v[b] = v[b], v[a]
            return f

        def short_credit(v):
            v[2].nValue = CTxOutValue(v[2].nValue.getAmount() - 1)
            v[3].nValue = CTxOutValue(v[3].nValue.getAmount() + 1)

        def rest_elsewhere(v):
            v[1].scriptPubKey = bytes.fromhex(taker)

        def rest_below_min_lot(v):
            moved = v[1].nValue.getAmount() - (MIN_LOT - 1)
            v[1].nValue = CTxOutValue(MIN_LOT - 1)
            v[4].nValue = CTxOutValue(v[4].nValue.getAmount() + moved)

        refusals = [
            ("credits swapped between 0 and 2", swap(0, 2), {0: "does not hold the asset the order is paid in", 1: "does not hold the asset the order is paid in"},
             "Script failed an OP_EQUALVERIFY operation"),
            ("order 2's credit one atom short", short_credit, {1: "below the ceiling price"},
             "Script evaluated without error but finished with a false/empty top stack element"),
            ("order 2's gap holds the asset the order sells", swap(3, 4), {1: "does not pay the order's own script"},
             "Script failed an OP_EQUALVERIFY operation"),
            ("order 1's rest paid to another script", rest_elsewhere, {0: "does not pay the order's own script"},
             "Script failed an OP_EQUALVERIFY operation"),
            ("order 1's rest below min_lot", rest_below_min_lot, {0: "below min_lot"},
             "Script failed an OP_VERIFY operation"),
        ]
        for name, change, refused_by, script_error in refusals:
            bad = mutate(change)
            for k, (terms, locked) in enumerate(((o1, l1), (o2, l2))):
                c = self.check(terms, bad, k, locked)
                if k in refused_by:
                    assert not c["ok"] and refused_by[k] in c["error"], (name, k, c)
                else:
                    assert c["ok"], (name, k, c)
            err = self.forced_block(bad)
            assert err is not None, "%s: MINED" % name
            assert script_error in err, (name, err)
            self.log.info("REJECT %s: CheckFill %s; block refused: %s", name,
                          "; ".join("order %d: %s" % (k + 1, self.check((o1, o2)[k], bad, k, (l1, l2)[k])["error"]) for k in refused_by),
                          err)

        err = self.forced_block(good)
        assert err is None, err
        txid = tx_from_hex(good).rehash()
        tx = node.getrawtransaction(txid, True)
        assert tx["confirmations"] >= 1
        rest = node.gettxout(txid, 1)
        assert_equal(rest["scriptPubKey"]["hex"], spk1)
        assert_equal(satoshi_round(rest["value"]) * COIN, Decimal(l1 - f1))
        c2 = node.gettxout(txid, 2)
        assert_equal(c2["scriptPubKey"]["hex"], bytes(CScript([OP_1, maker2])).hex())
        assert_equal(c2["asset"], Z)
        self.log.info("PASS one block fills order 1 in part at k=0 and order 2 whole at k=1: %s; "
                      "order 1 rests again with %s X at %s:1", txid, rest["value"], txid)

    def check_vectors(self):
        """The planner's vectors decode on the node as the layouts they claim."""
        node = self.nodes[0]
        path = os.path.join(seqdex_dir(), "daemon", "pkg", "covenant", "testdata", "fill_vectors.json")
        with open(path) as f:
            vectors = json.load(f)["vectors"]
        for v in vectors:
            d = node.decoderawtransaction(v["expect"]["hex"])
            assert_equal(d["txid"], v["expect"]["txid"])
            n = len(v["takes"])
            assert_equal(len(d["vin"]), n + len(v["inputs"]))
            for k, t in enumerate(v["takes"]):
                assert_equal((d["vin"][k]["txid"], d["vin"][k]["vout"]), (t["txid"], t["vout"]))
                assert_equal(len(d["vin"][k]["txinwitness"]), 2)
                credit = d["vout"][2 * k]
                assert_equal(credit["asset"], bytes.fromhex(t["terms"]["asset_b"])[::-1].hex())
                assert_equal(credit["scriptPubKey"]["hex"], "5120" + t["terms"]["maker_prog"])
                p = v["expect"]["plans"][k]
                assert_equal(int(satoshi_round(credit["value"]) * COIN), p["required_b"])
            self.log.info("VECTOR %s: decodes as %s", v["name"], d["txid"])


if __name__ == "__main__":
    SeqObMultiFillTest().main()
