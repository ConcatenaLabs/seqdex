#!/usr/bin/env python3
# Copyright (c) 2026 The Sequentia developers
# Distributed under the MIT software license.
"""SeqOB chain-watcher: prove the safety layer that reconciles the keyless
relay's resting COVENANT order book to actual Sequentia chain state.

A covenant order is FUNDED on-chain (the taproot UTXO is the coin; the signed
offer on the relay only ADVERTISES its outpoint). The relay is keyless and never
watches the chain, so the book drifts. The chain-watcher (seqdex
internal/seqob/watcher, run as a goroutine inside seqobd when it is given
-node-rpc) reconciles the book to the node's CURRENT tip. This test drives the
PRODUCTION Go watcher against a regtest node and asserts, over the relay's REST
book, each reconciliation outcome:

  1. FULL FILL   — a covenant is filled fully on-chain -> the watcher records
                   the fill from the transaction's outputs (credit at 2k, gap at
                   2k+1), holds the order while the fill is not final, and
                   REMOVES it once the fill is final.
  2. PARTIAL     — a partial fill re-creates a smaller remainder covenant at a
                   new outpoint (txid:2k+1) -> watcher RE-RESTS the order at the
                   new outpoint, and that remainder is itself fillable.
  3. UNCONFIRMED — a fill is broadcast but NOT confirmed -> the order is held
                   pending (not declared filled); once confirmed it settles.
  5. UNDO        — a confirmed partial fill's block leaves the chain -> the
                   record is UNDONE, its trade retracted, and the order rests
                   again at its own outpoint with its whole size; the fill,
                   mined again, is recorded again.
  4. GHOST       — the funding is UNDONE (invalidateblock + a double-spend of the
                   funding input, so the covenant UTXO is gone at the tip and no
                   spender exists) -> watcher REMOVES it. This is the
                   anchoring-supremacy safety case (first principle #1: a
                   Bitcoin-driven reorg can vanish a covenant's funding UTXO in
                   real time, and a resting order backed by a UTXO that no longer
                   exists is the dangerous case a taker could try to fill).

This chain has no Bitcoin anchor, so a fill's depth is counted in the chain's
own blocks above it, against the relay's -fill-anchor-depth (2): a fill is
final two blocks after the block that holds it.

The FILL spends are built from the SAME production Go covenant builder the other
covenant tests use (daemon/pkg/covenant). Requires the Go toolchain and a configured, built
Sequentia node checkout (env GO_BIN / SEQUENTIA_DIR override the defaults; see
seqob_env.py).
"""

import base64
import json
import os
import socket
import subprocess
import time
import urllib.request
from decimal import Decimal

import seqob_env  # noqa: F401  (puts the node's test_framework on sys.path)
from test_framework.test_framework import BitcoinTestFramework, SkipTest
from test_framework.util import (
    assert_equal, satoshi_round, BITCOIN_ASSET, get_auth_cookie, rpc_port,
)
from test_framework.key import compute_xonly_pubkey, generate_privkey
from test_framework.messages import (
    COIN, COutPoint, CTransaction, CTxIn, CTxInWitness, CTxOut, CTxOutAsset,
    CTxOutValue, tx_from_hex,
)
from test_framework.script import CScript, OP_1

import seqob_covenant as cov

FEE = 5000
N = 90 * COIN
RATE_NUM, RATE_DEN = 1, 3
MIN_LOT = 5 * COIN
RBF_SEQ = 0xfffffffd  # BIP125 opt-in replace-by-fee


def ceil_price(filled):
    return (filled * RATE_NUM + RATE_DEN - 1) // RATE_DEN


def free_port():
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


def go_bin():
    return seqob_env.go_bin()


def seqdex_dir():
    return seqob_env.seqdex_dir()


class SeqObWatcherTest(BitcoinTestFramework):

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
        ]]

    def skip_test_if_missing_module(self):
        self.skip_if_no_wallet()
        # The Go daemons under test live in the seqdex repo, which a bare
        # checkout (CI included) does not have. Skip, rather than fail, when
        # the toolchain or the repo is absent: their absence says nothing
        # about the node.
        if not os.path.exists(go_bin()):
            raise SkipTest("Go toolchain not found at %s (set GO_BIN)" % go_bin())
        if not os.path.isdir(os.path.join(seqdex_dir(), "daemon")):
            raise SkipTest("seqdex daemon dir not found at %s (set SEQDEX_DIR)"
                           % os.path.join(seqdex_dir(), "daemon"))

    def setup_network(self, split=False):
        self.setup_nodes()

    # --- build + run the Go relay (with the embedded watcher) ---------------

    def build_binaries(self):
        go = go_bin()
        sdir = os.path.join(seqdex_dir(), "daemon")
        if not os.path.exists(go):
            raise RuntimeError("Go toolchain not found at %s (set GO_BIN)" % go)
        if not os.path.isdir(sdir):
            raise RuntimeError("seqdex daemon dir not found at %s (set SEQDEX_DIR)" % sdir)
        outdir = os.path.join(self.options.tmpdir, "seqob-bins")
        os.makedirs(outdir, exist_ok=True)
        env = dict(os.environ)
        env["PATH"] = os.path.dirname(go) + os.pathsep + env.get("PATH", "")
        bins = {}
        for name, pkg in [
            ("seqobd", "./cmd/seqobd"),
            ("seqob-covenant", "./cmd/seqob-covenant"),
            ("seqob-relaycli", "./cmd/seqob-relaycli"),
            ("seqob-watcher", "./cmd/seqob-watcher"),
        ]:
            out = os.path.join(outdir, name)
            self.log.info("building %s ...", name)
            subprocess.run([go, "build", "-o", out, pkg], cwd=sdir, env=env, check=True)
            bins[name] = out
        return bins

    def node_rpc(self):
        user, password = get_auth_cookie(self.nodes[0].datadir, self.chain)
        return "127.0.0.1:%d" % rpc_port(self.nodes[0].index), user, password

    def start_relay(self):
        self.relay_port = free_port()
        self.relay_http = "http://127.0.0.1:%d" % self.relay_port
        host_port, user, password = self.node_rpc()
        env = dict(os.environ)
        env["SEQOB_LISTEN"] = ":%d" % self.relay_port
        self.relay_log = open(os.path.join(self.options.tmpdir, "seqobd.log"), "w")
        self.relay_proc = subprocess.Popen(
            [self.bins["seqobd"], "-min-expiry", "5s",
             "-node-rpc", host_port,
             "-node-rpc-user", user, "-node-rpc-pass", password,
             "-watch-interval", "1s"],
            env=env, stdout=self.relay_log, stderr=subprocess.STDOUT)
        for _ in range(100):
            try:
                s = socket.create_connection(("127.0.0.1", self.relay_port), timeout=0.2)
                s.close()
                return
            except OSError:
                time.sleep(0.1)
        raise RuntimeError("seqobd did not come up on port %d" % self.relay_port)

    def stop_relay(self):
        if getattr(self, "relay_proc", None):
            self.relay_proc.terminate()
            try:
                self.relay_proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.relay_proc.kill()
        if getattr(self, "relay_log", None):
            self.relay_log.close()

    # --- Go covenant builder -------------------------------------------------

    def cov_fill(self, locked, filled, k):
        out = subprocess.run(
            [self.bins["seqob-covenant"], "fill",
             "-a", self.asset_a.hex(), "-b", self.asset_b.hex(),
             "-num", str(RATE_NUM), "-den", str(RATE_DEN), "-minlot", str(MIN_LOT),
             "-prog", self.maker_x.hex(), "-makerver", "1",
             "-expiry", str(self.expiry), "-makerx", self.maker_x.hex(),
             "-locked", str(locked), "-filled", str(filled), "-k", str(k)],
            capture_output=True, text=True, check=True)
        return json.loads(out.stdout)

    def relay_post(self, priv, offer):
        p = subprocess.run(
            [self.bins["seqob-relaycli"], "post", "--http", self.relay_http,
             "--priv", priv.hex()],
            input=json.dumps(offer), capture_output=True, text=True, check=True)
        return json.loads(p.stdout)

    def orderbook(self):
        url = "%s/v1/market/%s/%s/orderbook" % (self.relay_http, self.A_display, self.B_display)
        with urllib.request.urlopen(url, timeout=5) as r:
            return json.loads(r.read().decode())

    def offer_in_book(self, offer_id):
        for o in self.orderbook().get("offers", []):
            if o["offer_id"] == offer_id:
                return o
        return None

    def fills(self):
        url = "%s/v1/market/%s/%s/fills" % (self.relay_http, self.A_display, self.B_display)
        with urllib.request.urlopen(url, timeout=5) as r:
            return json.loads(r.read().decode())["fills"]

    def trades(self):
        url = "%s/v1/market/%s/%s/trades" % (self.relay_http, self.A_display, self.B_display)
        with urllib.request.urlopen(url, timeout=5) as r:
            return json.loads(r.read().decode())["trades"]

    def wait_fills(self, predicate, what, timeout=25):
        deadline = time.time() + timeout
        last = None
        while time.time() < deadline:
            last = self.fills()
            if predicate(last):
                return last
            time.sleep(0.5)
        raise AssertionError("timed out waiting for: %s (last fills: %s)" % (what, json.dumps(last)))

    def fill_of(self, fills, offer_id, txid):
        return [f for f in fills if f["offer_id"] == offer_id and f["txid"] == txid]

    def wait_book(self, predicate, what, timeout=25):
        """Poll the relay book until predicate(book) is truthy (the watcher runs
        every 1s inside seqobd)."""
        deadline = time.time() + timeout
        last = None
        while time.time() < deadline:
            last = self.orderbook()
            if predicate(last):
                return last
            time.sleep(0.5)
        raise AssertionError("timed out waiting for: %s (last book: %s)" % (what, json.dumps(last)))

    # --- offer builders ------------------------------------------------------

    def now(self):
        return int(time.time())

    def covenant_offer(self, offer_id, txid, vout):
        req_full = ceil_price(N)
        b64 = lambda b: base64.b64encode(b).decode()
        return {
            "offer_id": offer_id, "schema_version": 1,
            "pair": {"base_asset": self.A_display, "quote_asset": self.B_display},
            "trade_dir": "TRADE_DIR_SELL",
            "base_amount": str(N), "offer_amount": str(N), "offer_asset": self.A_display,
            "want_amount": str(req_full), "want_asset": self.B_display,
            "allow_partial": True, "min_fill": str(MIN_LOT),
            "created_at_unix": str(self.now()), "expires_at_unix": str(self.now() + 3600),
            "covenant": {
                "covenant_txid": txid, "covenant_vout": vout,
                "asset_a": self.asset_a.hex(), "asset_b": self.asset_b.hex(),
                "rate_num": str(RATE_NUM), "rate_den": str(RATE_DEN),
                "maker_prog": b64(self.maker_x), "maker_prog_ver": 1,
                "min_lot": str(MIN_LOT), "expiry_locktime": self.expiry,
                "maker_x": b64(self.maker_x), "internal_key": b64(cov.NUMS),
                "merkle_path": [b64(self.order_tap.leaves["refund"].leaf_hash)],
            },
        }

    # --- on-chain helpers ----------------------------------------------------

    def wallet_spk(self):
        addr = self.nodes[0].getnewaddress()
        unconf = self.nodes[0].getaddressinfo(addr)["unconfidential"]
        return bytes.fromhex(self.nodes[0].getaddressinfo(unconf)["scriptPubKey"])

    def asset_out(self, display_hex):
        return b"\x01" + bytes.fromhex(display_hex)[::-1]

    def ctxout(self, amount, spk, asset_out):
        return CTxOut(nValue=CTxOutValue(amount), scriptPubKey=spk, nAsset=CTxOutAsset(asset_out))

    def fresh_segwit_utxo(self, amount, asset_display=None):
        node = self.nodes[0]
        bech = node.getnewaddress("", "bech32")
        unconf = node.getaddressinfo(bech)["unconfidential"]
        if asset_display is None:
            # SEQUENTIA: an open-fee-market chain has no default fee asset.
            node.sendtoaddress(address=unconf, amount=amount,
                               fee_asset_label=BITCOIN_ASSET)
            target = BITCOIN_ASSET
        else:
            node.sendtoaddress(address=unconf, amount=amount, assetlabel=asset_display,
                               fee_asset_label=BITCOIN_ASSET)
            target = asset_display
        self.generate(node, 1)
        for u in node.listunspent():
            if (u["asset"] == target and abs(float(u["amount"]) - amount) < 1e-9
                    and u["scriptPubKey"].startswith("0014") and u["spendable"]):
                return u
        raise AssertionError("no fresh segwit utxo for %s" % target)

    def find_asset_utxo(self, asset_display, min_units):
        for u in self.nodes[0].listunspent():
            if u["asset"] == asset_display and float(u["amount"]) >= min_units and u["spendable"]:
                return u
        raise AssertionError("no wallet utxo of %s" % asset_display)

    def find_btc_utxo(self, min_units, exclude):
        """A confirmed spendable BTC segwit utxo, WITHOUT mining (mining would
        re-include a resurrected mempool tx). exclude is a set of (txid,vout)."""
        for u in self.nodes[0].listunspent():
            if (u["asset"] == BITCOIN_ASSET and float(u["amount"]) >= min_units
                    and u["spendable"] and u["scriptPubKey"].startswith("0014")
                    and (u["txid"], u["vout"]) not in exclude):
                return u
        raise AssertionError("no spare BTC segwit utxo >= %s" % min_units)

    def fund_covenant(self, rbf=False):
        """Fund ONE covenant order UTXO (N of asset A paying order_spk). Returns
        (txid, vout, N, a_utxo, btc_utxo). With rbf=True the inputs opt into
        BIP125 so the funding can be double-spent for the GHOST case."""
        node = self.nodes[0]
        a_utxo = self.find_asset_utxo(self.A_display, 91)
        a_in = int(satoshi_round(a_utxo["amount"]) * COIN)
        btc = self.fresh_segwit_utxo(1)
        btc_in = int(satoshi_round(btc["amount"]) * COIN)

        seq = RBF_SEQ if rbf else 0xffffffff
        tx = CTransaction()
        tx.nVersion = 2
        tx.vin.append(CTxIn(COutPoint(int(a_utxo["txid"], 16), a_utxo["vout"]), nSequence=seq))
        tx.vin.append(CTxIn(COutPoint(int(btc["txid"], 16), btc["vout"]), nSequence=seq))
        order_spk = bytes(self.order_tap.scriptPubKey)
        tx.vout.append(self.ctxout(N, order_spk, self.A_OUT))
        tx.vout.append(self.ctxout(a_in - N, self.wallet_spk(), self.A_OUT))
        tx.vout.append(self.ctxout(btc_in - FEE, self.wallet_spk(), self.BTC_OUT))
        tx.vout.append(CTxOut(CTxOutValue(FEE)))
        signed = node.signrawtransactionwithwallet(tx.serialize().hex())
        assert signed["complete"], signed
        txid = node.sendrawtransaction(signed["hex"])
        self.generate(node, 1)
        return txid, 0, N, a_utxo, btc

    def assemble_fill(self, cov_in, wallet_ins, outs, witness):
        node = self.nodes[0]
        tx = CTransaction()
        tx.nVersion = 2
        tx.vin.append(CTxIn(COutPoint(int(cov_in[0], 16), cov_in[1])))
        for u in wallet_ins:
            tx.vin.append(CTxIn(COutPoint(int(u["txid"], 16), u["vout"])))
        for (amt, spk, aout) in outs:
            if aout is None:
                tx.vout.append(CTxOut(CTxOutValue(amt)))
            else:
                tx.vout.append(self.ctxout(amt, spk, aout))
        partial = node.signrawtransactionwithwallet(tx.serialize().hex())
        tx = tx_from_hex(partial["hex"])
        while len(tx.wit.vtxinwit) < len(tx.vin):
            tx.wit.vtxinwit.append(CTxInWitness())
        tx.wit.vtxinwit[0].scriptWitness.stack = witness
        return tx

    def build_full_fill(self, cov_txid, cov_vout, locked):
        """Build (but do not broadcast) a FULL fill spend of a covenant."""
        maker_spk = bytes(CScript([OP_1, self.maker_x]))
        plan = self.cov_fill(locked=locked, filled=locked, k=0)
        req = ceil_price(locked)
        witness = [bytes.fromhex(plan["fill_leaf"]), bytes.fromhex(plan["control_block"])]
        b_in = self.fresh_segwit_utxo(100, self.B_display)
        b_amt = int(satoshi_round(b_in["amount"]) * COIN)
        btc = self.fresh_segwit_utxo(1)
        btc_amt = int(satoshi_round(btc["amount"]) * COIN)
        tx = self.assemble_fill((cov_txid, cov_vout), [b_in, btc], [
            (req, maker_spk, self.B_OUT),
            (b_amt - req, self.wallet_spk(), self.B_OUT),
            (locked, self.wallet_spk(), self.A_OUT),
            (btc_amt - FEE, self.wallet_spk(), self.BTC_OUT),
            (FEE, b"", None),
        ], witness)
        return tx.serialize().hex()

    # --- the test ------------------------------------------------------------

    def run_test(self):
        node = self.nodes[0]
        self.bins = self.build_binaries()
        self.start_relay()
        try:
            self._run(node)
        finally:
            self.stop_relay()

    def _setup_assets(self, node):
        self.generate(node, 101)
        node.sendtoaddress(address=node.getnewaddress(), amount=1000000,
                           fee_asset_label=BITCOIN_ASSET)
        self.generate(node, 1)
        self.A_display = node.issueasset(assetamount=100000, tokenamount=0,
                                          blind=False, fee_asset=BITCOIN_ASSET)["asset"]
        self.generate(node, 1)
        self.B_display = node.issueasset(assetamount=100000, tokenamount=0,
                                          blind=False, fee_asset=BITCOIN_ASSET)["asset"]
        self.generate(node, 1)
        self.A_OUT = self.asset_out(self.A_display)
        self.B_OUT = self.asset_out(self.B_display)
        self.BTC_OUT = b"\x01" + bytes.fromhex(BITCOIN_ASSET)[::-1]
        self.asset_a = bytes.fromhex(self.A_display)[::-1]
        self.asset_b = bytes.fromhex(self.B_display)[::-1]

        maker_sec = generate_privkey()
        self.maker_x = compute_xonly_pubkey(maker_sec)[0]
        self.expiry = node.getblockcount() + 300
        self.order_tap, self.fill, self.refund = cov.order_taptree(
            self.asset_a, self.asset_b, RATE_NUM, RATE_DEN, self.maker_x, MIN_LOT,
            self.expiry, self.maker_x)

        self.maker_priv = bytes.fromhex("11" * 32)

    def _run(self, node):
        self._setup_assets(node)
        maker_spk = bytes(CScript([OP_1, self.maker_x]))

        # ================= SCENARIO 1: FULL FILL -> REMOVE =================
        self.log.info("SCENARIO 1: full on-chain fill -> watcher removes the resting order")
        txid, vout, locked, _, _ = self.fund_covenant()
        self.relay_post(self.maker_priv, self.covenant_offer("full", txid, vout))
        self.wait_book(lambda b: any(o["offer_id"] == "full" for o in b.get("offers", [])),
                       "covenant 'full' resting")
        # Fill it fully on-chain (maker offline) and confirm.
        ftxid = node.sendrawtransaction(self.build_full_fill(txid, vout, locked))
        self.generate(node, 1)
        rec = self.wait_fills(lambda fs: self.fill_of(fs, "full", ftxid), "the record of 'full''s fill")
        rec = self.fill_of(rec, "full", ftxid)[0]
        assert_equal((rec["input"], rec["credit"]["vout"], rec["state"]), (0, 0, "confirmed"))
        assert_equal(rec["filled"], locked)
        assert_equal(rec["credit"]["script"], maker_spk.hex())
        assert_equal(rec["gap"]["vout"], 1)
        assert_equal(rec["taker"]["via"], "chain")
        assert self.offer_in_book("full") is not None, "a fill not yet final must not remove the order"
        self.log.info("  record: input %d, credit %d at output %d, gap at %d, %s (depth %d of %d); order held",
                      rec["input"], rec["credit"]["value"], rec["credit"]["vout"], rec["gap"]["vout"],
                      rec["state"], rec["anchor_depth"], rec["required_depth"])
        self.generate(node, 2)
        self.wait_book(lambda b: all(o["offer_id"] != "full" for o in b.get("offers", [])),
                       "watcher removes fully-filled 'full' once its fill is final")
        self.wait_fills(lambda fs: any(f["state"] == "final" for f in self.fill_of(fs, "full", ftxid)), "'full''s fill final")
        self.log.info("  OK: watcher removed the fully-filled covenant order once its fill was final")

        # ================= SCENARIO 2: PARTIAL -> RE-REST =================
        self.log.info("SCENARIO 2: partial fill -> watcher re-rests the remainder at its new outpoint")
        txid2, vout2, _, _, _ = self.fund_covenant()
        self.relay_post(self.maker_priv, self.covenant_offer("part", txid2, vout2))
        self.wait_book(lambda b: any(o["offer_id"] == "part" for o in b.get("offers", [])),
                       "covenant 'part' resting")
        # Partial fill: take f1, re-create a rem1 remainder covenant at output 1.
        f1 = 30 * COIN
        plan1 = self.cov_fill(locked=N, filled=f1, k=0)
        assert_equal(plan1["partial"], True)
        assert_equal(int(plan1["remainder_index"]), 1)
        req1, rem1 = ceil_price(f1), N - f1
        wit1 = [bytes.fromhex(plan1["fill_leaf"]), bytes.fromhex(plan1["control_block"])]
        order_spk = bytes(self.order_tap.scriptPubKey)
        b_in = self.fresh_segwit_utxo(100, self.B_display)
        b_amt = int(satoshi_round(b_in["amount"]) * COIN)
        btc = self.fresh_segwit_utxo(1)
        btc_amt = int(satoshi_round(btc["amount"]) * COIN)
        tx = self.assemble_fill((txid2, vout2), [b_in, btc], [
            (req1, maker_spk, self.B_OUT),
            (rem1, order_spk, self.A_OUT),                     # 1: remainder -> SAME covenant
            (f1, self.wallet_spk(), self.A_OUT),
            (b_amt - req1, self.wallet_spk(), self.B_OUT),
            (btc_amt - FEE, self.wallet_spk(), self.BTC_OUT),
            (FEE, b"", None),
        ], wit1)
        ptxid = node.sendrawtransaction(tx.serialize().hex())
        self.generate(node, 1)
        assert_equal(satoshi_round(node.gettxout(ptxid, 1)["value"]) * COIN, Decimal(rem1))
        # Watcher must re-rest 'part' pointing at the NEW remainder outpoint.
        self.wait_book(
            lambda b: any(o["offer_id"] == "part" and o.get("covenant", {}).get("covenant_txid") == ptxid
                          and int(o.get("covenant", {}).get("covenant_vout", 0)) == 1
                          for o in b.get("offers", [])),
            "watcher re-rests 'part' at the remainder outpoint %s:1" % ptxid)
        self.log.info("  OK: watcher re-rested the remainder at %s:1 (reduced to %d units)", ptxid, rem1 // COIN)

        # The re-rested remainder is itself a valid covenant: fill it and watch removal.
        f2 = rem1  # take the whole remainder
        plan2 = self.cov_fill(locked=rem1, filled=f2, k=0)
        req2 = ceil_price(f2)
        wit2 = [bytes.fromhex(plan2["fill_leaf"]), bytes.fromhex(plan2["control_block"])]
        b_in = self.fresh_segwit_utxo(100, self.B_display)
        b_amt = int(satoshi_round(b_in["amount"]) * COIN)
        btc = self.fresh_segwit_utxo(1)
        btc_amt = int(satoshi_round(btc["amount"]) * COIN)
        tx = self.assemble_fill((ptxid, 1), [b_in, btc], [
            (req2, maker_spk, self.B_OUT),
            (b_amt - req2, self.wallet_spk(), self.B_OUT),
            (f2, self.wallet_spk(), self.A_OUT),
            (btc_amt - FEE, self.wallet_spk(), self.BTC_OUT),
            (FEE, b"", None),
        ], wit2)
        node.sendrawtransaction(tx.serialize().hex())
        self.generate(node, 3)
        self.wait_book(lambda b: all(o["offer_id"] != "part" for o in b.get("offers", [])),
                       "watcher removes the re-filled remainder")
        self.log.info("  OK: re-rested remainder was fillable; watcher removed it after the fill")

        # ============ SCENARIO 3: UNCONFIRMED SPEND -> HOLD, then CONFIRM ============
        self.log.info("SCENARIO 3: fill broadcast but NOT confirmed -> order held pending; then confirmed -> settles")
        txid3, vout3, locked3, _, _ = self.fund_covenant()
        self.relay_post(self.maker_priv, self.covenant_offer("pend", txid3, vout3))
        self.wait_book(lambda b: any(o["offer_id"] == "pend" for o in b.get("offers", [])),
                       "covenant 'pend' resting")
        raw_fill = self.build_full_fill(txid3, vout3, locked3)
        node.sendrawtransaction(raw_fill)  # in mempool, NOT mined
        # While unconfirmed, the watcher must NOT declare it filled/removed; it holds
        # it pending (hidden from matching). It must still be present in the book.
        time.sleep(4)  # let a few watcher passes run against the mempool spend
        assert self.offer_in_book("pend") is not None, \
            "an unconfirmed fill must not remove the order (mempool spend is reversible)"
        self.log.info("  OK: order stayed present (held pending) while the fill was only in the mempool")
        # Now confirm the fill, and bury it -> watcher removes it.
        self.generate(node, 3)
        self.wait_book(lambda b: all(o["offer_id"] != "pend" for o in b.get("offers", [])),
                       "watcher removes 'pend' once the fill confirms")
        self.log.info("  OK: once the fill confirmed, the watcher settled/removed the order")

        # ================= SCENARIO 4: GHOST (anchoring supremacy) =================
        self.log.info("SCENARIO 4: funding UNDONE by reorg -> watcher marks GHOST and removes it")
        txid4, vout4, _, a_utxo, btc_utxo = self.fund_covenant(rbf=True)
        funding_block = node.getbestblockhash()
        self.relay_post(self.maker_priv, self.covenant_offer("ghost", txid4, vout4))
        self.wait_book(lambda b: any(o["offer_id"] == "ghost" for o in b.get("offers", [])),
                       "covenant 'ghost' resting (LIVE)")
        self.log.info("  funded + resting; now UNDOING the funding (invalidateblock + double-spend of the input)")

        # Anchoring supremacy: a Bitcoin-driven reorg orphans the block that funded
        # the covenant, and the funding tx is NOT re-mined because its input is
        # double-spent in the new chain. The covenant UTXO vanishes at the tip.
        # invalidateblock resurrects the funding tx into the mempool; a
        # double-spend of its asset-A input, at a strictly higher fee (BIP125 RBF,
        # the funding opted in via rbf=True), evicts it so it can never be re-mined.
        # NB: do NOT mine here (a generate would re-include the resurrected funding).
        node.invalidateblock(funding_block)
        big_fee = 20 * FEE
        a_in = int(satoshi_round(a_utxo["amount"]) * COIN)
        btc_ds = self.find_btc_utxo(1, exclude={(a_utxo["txid"], a_utxo["vout"]),
                                                (btc_utxo["txid"], btc_utxo["vout"])})
        btc_ds_amt = int(satoshi_round(btc_ds["amount"]) * COIN)
        ds = CTransaction()
        ds.nVersion = 2
        ds.vin.append(CTxIn(COutPoint(int(a_utxo["txid"], 16), a_utxo["vout"]), nSequence=0xffffffff))
        ds.vin.append(CTxIn(COutPoint(int(btc_ds["txid"], 16), btc_ds["vout"])))
        ds.vout.append(self.ctxout(a_in, self.wallet_spk(), self.A_OUT))    # send A back to wallet
        ds.vout.append(self.ctxout(btc_ds_amt - big_fee, self.wallet_spk(), self.BTC_OUT))
        ds.vout.append(CTxOut(CTxOutValue(big_fee)))
        prevtxs = [{
            "txid": a_utxo["txid"], "vout": a_utxo["vout"],
            "scriptPubKey": a_utxo["scriptPubKey"], "amount": a_utxo["amount"],
            "asset": a_utxo["asset"],
        }]
        signed = node.signrawtransactionwithwallet(ds.serialize().hex(), prevtxs)
        assert signed["complete"], signed
        node.sendrawtransaction(signed["hex"])  # RBF-replaces the resurrected funding tx
        self.generate(node, 2)  # new tip WITHOUT the funding tx
        # The covenant funding outpoint no longer exists at the tip and has no spender.
        assert node.gettxout(txid4, vout4, True) is None, \
            "GHOST setup failed: the covenant outpoint is still a UTXO"
        self.wait_book(lambda b: all(o["offer_id"] != "ghost" for o in b.get("offers", [])),
                       "watcher marks the funding-undone order GHOST and removes it")
        self.log.info("  OK: watcher removed the GHOST order whose funding was undone by the reorg "
                      "(anchoring-supremacy safety case)")

        # ================= SCENARIO 5: a confirmed fill UNDONE by a reorg =================
        self.log.info("SCENARIO 5: a confirmed partial fill's block leaves the chain -> the record is undone, the order rests again")
        txid5, vout5, _, _, _ = self.fund_covenant()
        self.relay_post(self.maker_priv, self.covenant_offer("undo", txid5, vout5))
        self.wait_book(lambda b: any(o["offer_id"] == "undo" for o in b.get("offers", [])), "covenant 'undo' resting")
        f5 = 30 * COIN
        plan5 = self.cov_fill(locked=N, filled=f5, k=0)
        req5, rem5 = ceil_price(f5), N - f5
        wit5 = [bytes.fromhex(plan5["fill_leaf"]), bytes.fromhex(plan5["control_block"])]
        b_in = self.fresh_segwit_utxo(100, self.B_display)
        b_amt = int(satoshi_round(b_in["amount"]) * COIN)
        btc = self.fresh_segwit_utxo(1)
        btc_amt = int(satoshi_round(btc["amount"]) * COIN)
        tx = self.assemble_fill((txid5, vout5), [b_in, btc], [
            (req5, maker_spk, self.B_OUT),
            (rem5, order_spk, self.A_OUT),
            (f5, self.wallet_spk(), self.A_OUT),
            (b_amt - req5, self.wallet_spk(), self.B_OUT),
            (btc_amt - FEE, self.wallet_spk(), self.BTC_OUT),
            (FEE, b"", None),
        ], wit5)
        utxid = node.sendrawtransaction(tx.serialize().hex())
        fill_block = self.generate(node, 1)[0]
        self.wait_book(lambda b: any(o["offer_id"] == "undo" and o["covenant"]["covenant_txid"] == utxid for o in b.get("offers", [])),
                       "'undo' re-rested at %s:1" % utxid)
        trades_before = len(self.trades())
        rec = self.fill_of(self.fills(), "undo", utxid)[0]
        assert_equal((rec["state"], rec["rest"]["vout"], rec["remainder"]), ("confirmed", 1, rem5))
        node.invalidateblock(fill_block)  # the fill goes back to the mempool
        self.wait_fills(lambda fs: any(f["state"] == "undone" for f in self.fill_of(fs, "undo", utxid)), "'undo''s fill undone")
        rec = [f for f in self.fill_of(self.fills(), "undo", utxid) if f["state"] == "undone"][0]
        self.log.info("  record undone: %s", rec["reason"])
        assert "left the active chain" in rec["reason"], rec
        self.wait_book(lambda b: any(o["offer_id"] == "undo" and o["covenant"]["covenant_txid"] == txid5 for o in b.get("offers", [])),
                       "'undo' back at its own outpoint %s:%d" % (txid5, vout5))
        assert_equal(len(self.trades()), trades_before - 1)
        self.log.info("  OK: the order rests again at %s:%d and its trade is retracted", txid5, vout5)
        # Mined again: recorded again, as a new fill, and re-rested again.
        node.reconsiderblock(fill_block)
        self.wait_fills(lambda fs: any(f["state"] in ("confirmed", "final") for f in self.fill_of(fs, "undo", utxid)),
                        "'undo''s fill recorded again")
        self.wait_book(lambda b: any(o["offer_id"] == "undo" and o["covenant"]["covenant_txid"] == utxid for o in b.get("offers", [])),
                       "'undo' re-rested again at %s:1" % utxid)
        assert_equal(len(self.trades()), trades_before)
        self.log.info("  OK: mined again, the fill is recorded again and the order rests at %s:1", utxid)

        self.log.info("SeqOB chain-watcher proven: the resting covenant book is reconciled to actual "
                      "chain state — fills recorded and removed once final, partials re-rested, "
                      "unconfirmed spends held, fills whose block leaves the chain undone, and "
                      "reorg-vanished funding (ghosts) removed.")


if __name__ == "__main__":
    SeqObWatcherTest().main()
