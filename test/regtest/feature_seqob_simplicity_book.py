#!/usr/bin/env python3
# Copyright (c) 2026 The Sequentia developers
# Distributed under the MIT software license.
"""A Simplicity order in SeqDEX's book: posted, validated, filled, followed.

A Simplicity order (the template seqdex/order, pkg/covenant SimOrder) rests
under its own terms member, SimplicityCovenantTerms. This drives the
production relay (seqobd, with its chain watcher), the maker's posting tool
(seqob-relaycli) and the taker (seqob-cli covfill) against a regtest node with
Simplicity active from genesis, and asserts over the relay's REST API:

  POST    the order, whose fill product (2e13 atoms at a 13-digit price) the
          tapscript leaf cannot carry, is validated, probed on the chain and
          served in the book; a post naming another template, and one whose
          outpoint holds other than offer_amount, are refused for their reasons
  PARTIAL seqob-cli covfill takes 6e12 of it at input 0: the watcher records
          the fill from its outputs (credit at 0, rest at 1), serves the order
          at (fill txid, 1), and marks the fill final two blocks later
  WHOLE   covfill takes the rest: the watcher holds the order until that fill
          is final, then removes it; both trades are in the feed

Every fill pays its fee in an issued asset; none touches the policy asset.
Requires the Go toolchain and a configured, built Sequentia node checkout; see
seqob_env.py.
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
from test_framework.util import assert_equal, BITCOIN_ASSET, get_auth_cookie, rpc_port
from test_framework.address import program_to_witness
from test_framework.key import compute_xonly_pubkey, generate_privkey
from test_framework.messages import COIN

N = 20_000_000_000_000
RATE_NUM, RATE_DEN = 9_000_000_000_007, 10_000_000_000_000
MIN_LOT = 1_000_000
FEE = 20_000
INT63 = 2 ** 63 - 1


def ceil_price(filled):
    return -(-filled * RATE_NUM // RATE_DEN)


def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    p = s.getsockname()[1]
    s.close()
    return p


class SeqObSimplicityBookTest(BitcoinTestFramework):

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
            "-evbparams=simplicity:-1:::",
        ]]

    def skip_test_if_missing_module(self):
        self.skip_if_no_wallet()
        if not os.path.exists(seqob_env.go_bin()):
            raise SkipTest("Go toolchain not found at %s (set GO_BIN)" % seqob_env.go_bin())

    def setup_network(self, split=False):
        self.setup_nodes()

    # --- the Go binaries and the relay -----------------------------------------

    def build_binaries(self):
        go = seqob_env.go_bin()
        outdir = os.path.join(self.options.tmpdir, "seqob-bins")
        os.makedirs(outdir, exist_ok=True)
        env = dict(os.environ)
        env["PATH"] = os.path.dirname(go) + os.pathsep + env.get("PATH", "")
        bins = {}
        for name in ("seqobd", "seqob-covenant", "seqob-relaycli", "seqob-cli"):
            out = os.path.join(outdir, name)
            subprocess.run([go, "build", "-o", out, "./cmd/" + name],
                           cwd=os.path.join(seqob_env.seqdex_dir(), "daemon"), env=env, check=True)
            bins[name] = out
        return bins

    def start_relay(self):
        self.relay_port = free_port()
        self.relay_http = "http://127.0.0.1:%d" % self.relay_port
        user, password = get_auth_cookie(self.nodes[0].datadir, self.chain)
        self.rpc_auth = (user, password, rpc_port(self.nodes[0].index))
        env = dict(os.environ, SEQOB_LISTEN=":%d" % self.relay_port)
        self.relay_log = open(os.path.join(self.options.tmpdir, "seqobd.log"), "w")
        self.relay_proc = subprocess.Popen(
            [self.bins["seqobd"], "-min-expiry", "5s", "-node-rpc", "127.0.0.1:%d" % self.rpc_auth[2],
             "-node-rpc-user", user, "-node-rpc-pass", password, "-watch-interval", "1s"],
            env=env, stdout=self.relay_log, stderr=subprocess.STDOUT)
        for _ in range(100):
            try:
                socket.create_connection(("127.0.0.1", self.relay_port), timeout=0.2).close()
                return
            except OSError:
                time.sleep(0.1)
        raise RuntimeError("seqobd did not come up")

    def stop_relay(self):
        if getattr(self, "relay_proc", None):
            self.relay_proc.terminate()
            try:
                self.relay_proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.relay_proc.kill()
        if getattr(self, "relay_log", None):
            self.relay_log.close()

    def get(self, path):
        with urllib.request.urlopen(self.relay_http + path, timeout=5) as r:
            return json.loads(r.read().decode())

    def market(self, what):
        return self.get("/v1/market/%s/%s/%s" % (self.X, self.Y, what))

    def book_offer(self, offer_id):
        for o in self.market("orderbook").get("offers", []):
            if o["offer_id"] == offer_id:
                return o
        return None

    def wait(self, predicate, what, timeout=30):
        deadline = time.time() + timeout
        last = None
        while time.time() < deadline:
            last = predicate()
            if last:
                return last
            time.sleep(0.5)
        raise AssertionError("timed out waiting for: %s" % what)

    def post(self, offer, expect_refusal=None):
        p = subprocess.run([self.bins["seqob-relaycli"], "post", "--http", self.relay_http, "--priv", self.maker_priv],
                           input=json.dumps(offer), capture_output=True, text=True)
        if expect_refusal is None:
            assert p.returncode == 0, p.stderr
            return json.loads(p.stdout)
        assert p.returncode != 0 and expect_refusal in p.stderr, (expect_refusal, p.stderr)
        self.log.info("REFUSED post %s: %s", offer["offer_id"], p.stderr.strip()[-200:])
        return None

    def covfill(self, offer_id, amount):
        user, password, port = self.rpc_auth
        p = subprocess.run([self.bins["seqob-cli"], "covfill", "-relay", self.relay_http,
                            "-base", self.X, "-quote", self.Y, "-offer-id", offer_id,
                            "-maker-pubkey", self.maker_pub, "-amount", str(amount),
                            "-seq-rpc", "http://%s:%s@127.0.0.1:%d" % (user, password, port),
                            "-seq-wallet", self.wallet, "-fee-asset", self.Z, "-spend-fee", str(FEE)],
                           capture_output=True, text=True)
        assert p.returncode == 0, p.stdout + p.stderr
        self.log.info("covfill: %s", " | ".join(l for l in p.stdout.splitlines() if l.strip()))
        return [l.split()[-1] for l in p.stdout.splitlines() if l.startswith("SWAP SETTLED")][0]

    # --- the test ------------------------------------------------------------------

    def offer(self, offer_id, txid, vout, amount, template=None):
        b64 = lambda b: base64.b64encode(b).decode()
        now = int(time.time())
        return {
            "offer_id": offer_id, "schema_version": 1, "pair": {"base_asset": self.X, "quote_asset": self.Y},
            "trade_dir": "TRADE_DIR_SELL", "base_amount": str(amount), "offer_amount": str(amount),
            "offer_asset": self.X, "want_amount": str(amount * RATE_NUM // RATE_DEN), "want_asset": self.Y,
            "allow_partial": True, "min_fill": str(MIN_LOT),
            "created_at_unix": str(now), "expires_at_unix": str(now + 3600),
            "simplicity_covenant": {
                "covenant_txid": txid, "covenant_vout": vout,
                "asset_a": self.internal(self.X), "asset_b": self.internal(self.Y),
                "rate_num": str(RATE_NUM), "rate_den": str(RATE_DEN), "min_lot": str(MIN_LOT),
                "maker_script": b64(self.maker_spk), "expiry_locktime": self.expiry,
                "refund_key": b64(self.refund_x), "template_hash": template or self.template,
            },
        }

    @staticmethod
    def internal(display):
        return bytes.fromhex(display)[::-1].hex()

    def run_test(self):
        node = self.nodes[0]
        self.bins = self.build_binaries()
        self.generate(node, 101)
        node.sendtoaddress(address=node.getnewaddress(), amount=1000000, fee_asset_label=BITCOIN_ASSET)
        self.generate(node, 1)
        self.X, self.Y, self.Z = [node.issueasset(assetamount=1000000, tokenamount=0, blind=False,
                                                  fee_asset=BITCOIN_ASSET)["asset"] for _ in range(3)]
        self.generate(node, 1)
        rates = node.getfeeexchangerates()
        rates.update({self.Y: 100000000, self.Z: 100000000})
        node.setfeeexchangerates(rates)
        a = node.getnewaddress("", "bech32")
        self.maker_spk = bytes.fromhex(node.getaddressinfo(node.getaddressinfo(a)["unconfidential"])["scriptPubKey"])
        # The taker's own wallet, named, holding the asset it pays in and the fee asset.
        maker_wallet = node.listwallets()[0]
        node.createwallet(wallet_name="taker")
        w0, tw = node.get_wallet_rpc(maker_wallet), node.get_wallet_rpc("taker")
        self.wallet = "taker"
        for asset, atoms in ((self.Y, 30_000_000_000_000), (self.Z, 5_000_000)):
            dest = tw.getaddressinfo(tw.getnewaddress("", "bech32"))["unconfidential"]
            w0.sendtoaddress(address=dest, amount=Decimal(atoms) / COIN, assetlabel=asset, fee_asset_label=BITCOIN_ASSET)
        self.generate(node, 1)
        node = w0  # the maker's wallet, for the rest; blocks come from the node
        self.refund_x = compute_xonly_pubkey(generate_privkey())[0]
        self.expiry = node.getblockcount() + 1000
        self.maker_priv = "11" * 32
        from test_framework.key import ECKey
        k = ECKey()
        k.set(bytes.fromhex(self.maker_priv), True)
        self.maker_pub = k.get_pubkey().get_bytes().hex()
        assert N * RATE_NUM + RATE_DEN - 1 > INT63

        d = json.loads(subprocess.run(
            [self.bins["seqob-covenant"], "simderive", "-a", self.internal(self.X), "-b", self.internal(self.Y),
             "-num", str(RATE_NUM), "-den", str(RATE_DEN), "-minlot", str(MIN_LOT), "-maker", self.maker_spk.hex(),
             "-expiry", str(self.expiry), "-refund", self.refund_x.hex()],
            capture_output=True, text=True, check=True).stdout)
        self.template = d["template_hash"]
        spk = bytes.fromhex(d["scriptpubkey"])
        txid = node.sendtoaddress(address=program_to_witness(1, spk[2:]), amount=Decimal(N) / COIN,
                                  assetlabel=self.X, fee_asset_label=BITCOIN_ASSET)
        self.generate(self.nodes[0], 1)
        vout = [o["n"] for o in node.getrawtransaction(txid, True)["vout"] if o["scriptPubKey"]["hex"] == d["scriptpubkey"]][0]
        self.log.info("the order rests at %s:%d, %s", txid, vout, d["address"]["elementsregtest"])

        self.start_relay()
        try:
            self.post(self.offer("bad-template", txid, vout, N, template="00" * 32), "is not seqdex/order")
            self.post(self.offer("bad-amount", txid, vout, 10_000_000_000_000), "holds %d atoms, offer advertises 10000000000000" % N)
            self.post(self.offer("S", txid, vout, N))
            served = self.wait(lambda: self.book_offer("S"), "the order in the book")
            assert_equal(served["simplicity_covenant"]["covenant_txid"], txid)
            self.log.info("POST the order is served in the book at %s:%d", txid, vout)

            F1 = 6_000_000_000_000
            ftx = self.covfill("S", F1)
            self.generate(self.nodes[0], 1)
            fills = self.wait(lambda: [f for f in self.market("fills")["fills"] if f["txid"] == ftx], "the partial fill's record")
            f = fills[0]
            assert_equal((f["input"], f["credit"]["vout"], f["rest"]["vout"], f["filled"], f["remainder"]),
                         (0, 0, 1, F1, N - F1))
            assert_equal(f["credit"]["value"], ceil_price(F1))
            assert_equal(f["credit"]["script"], self.maker_spk.hex())
            moved = self.wait(lambda: (lambda o: o if o and o["simplicity_covenant"]["covenant_txid"] == ftx else None)(self.book_offer("S")),
                              "the order served at the fill's rest")
            assert_equal(moved["simplicity_covenant"]["covenant_vout"], 1)
            self.log.info("PARTIAL fill %s: credit %d at 0, rest %d at 1; the order is served at %s:1",
                          ftx, f["credit"]["value"], f["remainder"], ftx)
            self.generate(self.nodes[0], 2)
            self.wait(lambda: [x for x in self.market("fills")["fills"] if x["txid"] == ftx and x["state"] == "final"], "the partial fill final")

            wtx = self.covfill("S", 0)
            self.generate(self.nodes[0], 1)
            w = self.wait(lambda: [x for x in self.market("fills")["fills"] if x["txid"] == wtx], "the whole fill's record")[0]
            assert_equal((w["input"], w["filled"], w["remainder"]), (0, N - F1, 0))
            assert self.book_offer("S") is not None, "a fill not yet final must not remove the order"
            self.generate(self.nodes[0], 2)
            self.wait(lambda: self.book_offer("S") is None, "the order removed once its whole fill is final")
            self.wait(lambda: [x for x in self.market("fills")["fills"] if x["txid"] == wtx and x["state"] == "final"], "the whole fill final")
            trades = self.market("trades")["trades"]
            assert_equal(sorted(int(t["size"]) for t in trades), sorted([F1, N - F1]))
            for t in (ftx, wtx):
                self.log.info("SIZE fill %s: %d vB", t, node.getrawtransaction(t, True)["vsize"])
            self.log.info("WHOLE fill %s: the order removed once final; trades %s", wtx, [t["size"] for t in trades])
        finally:
            self.stop_relay()


if __name__ == "__main__":
    SeqObSimplicityBookTest().main()
