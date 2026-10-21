#!/usr/bin/env python3
# Copyright (c) 2026 The Sequentia developers
# Distributed under the MIT software license.
"""A part-filled covenant order stays listable across a relay restart.

A covenant order rests under its program at the outpoint its signed offer
names, and the outpoint is outside the maker's signature. A part fill
re-creates the order under the same program at the fill's output 1, holding
less than the signed offer_amount. The relay (seqobd) lists the signed offer
at such a remainder at what the outpoint holds, so after a restart anyone
holding the signed offer (its maker, a seeder, a deploy script) can post it
again, and the book shows only what can be filled.

On an anchored regtest chain (node 0 stands in for Bitcoin, node 1 anchors to
it), with the production relay and its chain watcher, the maker's posting tool
(seqob-relaycli) and the taker (seqob-cli covfill):

  LISTED  a tapscript order (NUMS key) posted and served; a part fill re-rests
          it at the fill's output 1 at the remainder's size
  RESTART the relay restarted: the book is empty
  ADOPTED the maker's signed bytes, unchanged, posted again naming the
          remainder: listed at the remainder's size
  REFUSED after another restart, the same signed offer naming an outpoint of
          another program, one holding more than offer_amount, one below
          min_lot, a confidential one and a spent one: each refused for its
          reason; one holding exactly min_lot is listed at min_lot (a whole
          fill takes it)
  TRADED  adopted again after a third restart, filled in part and then whole,
          the listed size following the outpoint's value each time

Every fill pays its fee in an issued asset; none touches the policy asset.
Requires the Go toolchain and a configured, built Sequentia node checkout; see
seqob_env.py. SEQOBD_BIN runs a prebuilt relay instead of this tree's.
"""

import base64
import copy
import json
import os
import socket
import subprocess
import time
import urllib.error
import urllib.request
from decimal import Decimal

import seqob_env  # noqa: F401  (puts the node's test_framework on sys.path)
from test_framework.test_framework import BitcoinTestFramework, SkipTest
from test_framework.util import (
    assert_equal, get_auth_cookie, get_datadir_path, p2p_port, rpc_port,
)
from test_framework.address import program_to_witness
from test_framework.key import ECKey, compute_xonly_pubkey, generate_privkey
from test_framework.messages import COIN

NUMS = "50929b74c1a04954b78b4b6035e97a5e078a5a0f28ec96d547bfee9ace803ac0"
N = 50_000_000
RATE_NUM, RATE_DEN = 2, 1
MIN_LOT = 1_000_000
F1, F2 = 20_000_000, 10_000_000
FEE = 20_000


def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    p = s.getsockname()[1]
    s.close()
    return p


class SeqObBookRemainderTest(BitcoinTestFramework):

    def set_test_params(self):
        self.setup_clean_chain = True
        self.num_nodes = 2

    def skip_test_if_missing_module(self):
        self.skip_if_no_wallet()
        if not os.path.exists(seqob_env.go_bin()):
            raise SkipTest("Go toolchain not found at %s (set GO_BIN)" % seqob_env.go_bin())

    def setup_network(self, split=False):
        # node 0: the parent chain, standing in for Bitcoin.
        parent_chain = "elementsregtest"
        self.add_nodes(1, [[
            "-port=%d" % p2p_port(0), "-rpcport=%d" % rpc_port(0), "-validatepegin=0",
            "-initialfreecoins=0", "-con_blocksubsidy=5000000000", "-anyonecanspendaremine=1",
            "-signblockscript=51",
        ]], chain=[parent_chain])
        self.start_node(0)
        u, p = get_auth_cookie(get_datadir_path(self.options.tmpdir, 0), parent_chain)
        # node 1: the anchored chain the order book runs on.
        self.add_nodes(1, [[
            "-port=%d" % p2p_port(1), "-rpcport=%d" % rpc_port(1), "-validatepegin=0",
            "-initialfreecoins=2100000000000000", "-anyonecanspendaremine=1", "-signblockscript=51",
            "-con_bitcoin_anchor=1", "-validateanchor=1", "-anchorpollinterval=1",
            "-mainchainrpchost=127.0.0.1", "-mainchainrpcport=%d" % rpc_port(0),
            "-mainchainrpcuser=%s" % u, "-mainchainrpcpassword=%s" % p,
            "-parentgenesisblockhash=%s" % self.nodes[0].getblockhash(0),
            "-blindedaddresses=0", "-con_default_blinded_addresses=0", "-con_any_asset_fees=1",
            "-maxtxfee=100.0", "-txindex=1", "-par=1",
        ]], chain=["elementsregtest"])
        self.start_node(1)
        for n in self.nodes:
            n.createwallet(wallet_name=self.default_wallet_name)
        self.nodes[1].rescanblockchain()  # the genesis's free coins (-anyonecanspendaremine)

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
        if os.environ.get("SEQOBD_BIN"):
            bins["seqobd"] = os.environ["SEQOBD_BIN"]
            self.log.info("relay: %s", bins["seqobd"])
        return bins

    def start_relay(self):
        self.relay_port = free_port()
        self.relay_http = "http://127.0.0.1:%d" % self.relay_port
        user, password = get_auth_cookie(self.nodes[1].datadir, "elementsregtest")
        self.rpc_auth = (user, password, rpc_port(1))
        env = dict(os.environ, SEQOB_LISTEN=":%d" % self.relay_port)
        self.relay_log = open(os.path.join(self.options.tmpdir, "seqobd.log"), "a")
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
            self.relay_proc = None
        if getattr(self, "relay_log", None):
            self.relay_log.close()
            self.relay_log = None

    def restart_relay(self, why):
        self.stop_relay()
        self.start_relay()
        assert_equal(self.market("orderbook").get("offers", []), [])
        self.log.info("RESTART (%s): the relay is back on :%d with an empty book", why, self.relay_port)

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
        while time.time() < deadline:
            last = predicate()
            if last:
                return last
            time.sleep(0.5)
        raise AssertionError("timed out waiting for: %s" % what)

    def post_signed(self, offer):
        """POST an offer as it is, signature and all: what a re-post after a restart does."""
        req = urllib.request.Request(self.relay_http + "/v1/offers", data=json.dumps(offer).encode(),
                                     headers={"Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=10) as r:
                return 200, json.loads(r.read().decode())
        except urllib.error.HTTPError as e:
            return e.code, json.loads(e.read().decode())

    def at(self, offer, txid, vout):
        o = copy.deepcopy(offer)
        o["covenant"]["covenant_txid"], o["covenant"]["covenant_vout"] = txid, vout
        return o

    def refused(self, what, offer, want):
        code, j = self.post_signed(offer)
        assert code == 400 and want in j.get("message", ""), (what, code, j)
        self.log.info("REFUSED %s: %s", what, j["message"])

    def listed_at(self, offer):
        """The live size of a resting order: a byte-identical re-post echoes its status."""
        code, st = self.post_signed(offer)
        assert_equal(code, 200)
        return int(st.get("active_amount", 0)), st.get("status")

    def adopt(self, signed, ftx):
        code, st = self.post_signed(self.at(signed, ftx, 1))
        assert code == 200, "the signed offer re-posted at its remainder %s:1 was refused: %s %s" % (ftx, code, st)
        assert_equal((st["status"], int(st["active_amount"])), ("OFFER_STATUS_PARTIAL", N - F1))
        served = self.book_offer("O")
        assert_equal((served["covenant"]["covenant_txid"], served["covenant"]["covenant_vout"], served["maker_sig"]),
                     (ftx, 1, signed["maker_sig"]))
        self.log.info("ADOPTED the signed offer re-posted at %s:1 is listed at %d (the outpoint holds %d)",
                      ftx, int(st["active_amount"]), self.held(ftx, 1))

    def held(self, txid, vout):
        u = self.nodes[1].gettxout(txid, vout, True)
        return round(Decimal(str(u["value"])) * COIN) if u else None

    def covfill(self, offer_id, amount):
        user, password, port = self.rpc_auth
        p = subprocess.run([self.bins["seqob-cli"], "covfill", "-relay", self.relay_http,
                            "-base", self.X, "-quote", self.Y, "-offer-id", offer_id,
                            "-maker-pubkey", self.maker_pub, "-amount", str(amount),
                            "-seq-rpc", "http://%s:%s@127.0.0.1:%d" % (user, password, port),
                            "-seq-wallet", "taker", "-fee-asset", self.Z, "-spend-fee", str(FEE)],
                           capture_output=True, text=True)
        assert p.returncode == 0, p.stdout + p.stderr
        self.log.info("covfill: %s", " | ".join(l for l in p.stdout.splitlines() if l.strip())[-300:])
        return [l.split()[-1] for l in p.stdout.splitlines() if l.startswith("SWAP SETTLED")][0]

    def mine(self, n=1):
        self.generatetoaddress(self.nodes[1], n, self.miner, sync_fun=self.no_op)

    def bury(self):
        """Two parent blocks over the last fill, and a block of ours that anchors them."""
        self.generatetoaddress(self.nodes[0], 2, self.parent_addr, sync_fun=self.no_op)
        time.sleep(2)
        self.mine(1)

    # --- the order ---------------------------------------------------------------

    @staticmethod
    def internal(display):
        return bytes.fromhex(display)[::-1].hex()

    def derive(self, num, den):
        return json.loads(subprocess.run(
            [self.bins["seqob-covenant"], "derive", "-a", self.internal(self.X), "-b", self.internal(self.Y),
             "-num", str(num), "-den", str(den), "-minlot", str(MIN_LOT), "-prog", self.maker_x.hex(),
             "-expiry", str(self.expiry), "-makerx", self.maker_x.hex()],
            capture_output=True, text=True, check=True).stdout)

    def fund(self, d, atoms, blinded=False):
        spk = bytes.fromhex(d["scriptpubkey"])
        addr = program_to_witness(1, spk[2:])
        if blinded:
            key = self.w.getaddressinfo(self.w.getnewaddress("", "blech32"))["confidential_key"]
            addr = self.w.createblindedaddress(addr, key)
        txid = self.w.sendtoaddress(address=addr, amount=Decimal(atoms) / COIN, assetlabel=self.X,
                                    fee_asset_label=self.policy)
        self.mine(1)
        vout = [o["n"] for o in self.w.getrawtransaction(txid, True)["vout"] if o["scriptPubKey"]["hex"] == d["scriptpubkey"]][0]
        return txid, vout

    def offer(self, offer_id, d, txid, vout):
        b64 = lambda b: base64.b64encode(b).decode()
        now = int(time.time())
        return {
            "offer_id": offer_id, "schema_version": 1, "pair": {"base_asset": self.X, "quote_asset": self.Y},
            "trade_dir": "TRADE_DIR_SELL", "base_amount": str(N), "offer_amount": str(N),
            "offer_asset": self.X, "want_amount": str(N * RATE_NUM // RATE_DEN), "want_asset": self.Y,
            "allow_partial": True, "min_fill": str(MIN_LOT),
            "created_at_unix": str(now), "expires_at_unix": str(now + 3600),
            "covenant": {
                "covenant_txid": txid, "covenant_vout": vout,
                "asset_a": self.internal(self.X), "asset_b": self.internal(self.Y),
                "rate_num": str(RATE_NUM), "rate_den": str(RATE_DEN),
                "maker_prog": b64(self.maker_x), "maker_prog_ver": 1,
                "min_lot": str(MIN_LOT), "expiry_locktime": self.expiry,
                "maker_x": b64(self.maker_x), "internal_key": b64(bytes.fromhex(NUMS)),
                "merkle_path": [b64(bytes.fromhex(d["refund_leaf_hash"]))],
            },
        }

    # --- the test ------------------------------------------------------------------

    def run_test(self):
        parent, node = self.nodes[0], self.nodes[1]
        self.bins = self.build_binaries()
        self.parent_addr = parent.getnewaddress()
        self.generatetoaddress(parent, 5, self.parent_addr, sync_fun=self.no_op)
        self.miner = node.getnewaddress()
        self.mine(101)
        # The anchored chain's own policy asset (its genesis commits the parent's).
        self.policy = node.dumpassetlabels()["bitcoin"]
        status = node.getanchorstatus()
        self.log.info("anchored: tip anchors parent block %s (%s)", status["anchorheight"], status.get("anchorstatus", status))
        node.sendtoaddress(address=node.getnewaddress(), amount=1000000, fee_asset_label=self.policy)
        self.mine(1)
        self.X, self.Y, self.Z = [node.issueasset(assetamount=1000000, tokenamount=0, blind=False,
                                                  fee_asset=self.policy)["asset"] for _ in range(3)]
        self.mine(1)
        rates = node.getfeeexchangerates()
        rates.update({self.Y: 100000000, self.Z: 100000000})
        node.setfeeexchangerates(rates)
        maker_wallet = node.listwallets()[0]
        node.createwallet(wallet_name="taker")
        self.w, tw = node.get_wallet_rpc(maker_wallet), node.get_wallet_rpc("taker")
        for asset, atoms in ((self.Y, 500_000_000), (self.Z, 5_000_000)):
            dest = tw.getaddressinfo(tw.getnewaddress("", "bech32"))["unconfidential"]
            self.w.sendtoaddress(address=dest, amount=Decimal(atoms) / COIN, assetlabel=asset, fee_asset_label=self.policy)
        self.mine(1)

        self.maker_x = compute_xonly_pubkey(generate_privkey())[0]
        self.expiry = node.getblockcount() + 1000
        self.maker_priv = "11" * 32
        k = ECKey()
        k.set(bytes.fromhex(self.maker_priv), True)
        self.maker_pub = k.get_pubkey().get_bytes().hex()

        d = self.derive(RATE_NUM, RATE_DEN)
        txid, vout = self.fund(d, N)
        self.log.info("the order rests at %s:%d under %s", txid, vout, d["scriptpubkey"])

        self.start_relay()
        try:
            self.run_book(d, txid, vout)
        finally:
            self.stop_relay()

    def run_book(self, d, txid, vout):
        node = self.nodes[1]
        # LISTED: the maker signs and posts once, then is offline. The signed
        # offer is public: the book serves it, maker_sig and all.
        p = subprocess.run([self.bins["seqob-relaycli"], "post", "--http", self.relay_http, "--priv", self.maker_priv],
                           input=json.dumps(self.offer("O", d, txid, vout)), capture_output=True, text=True)
        assert p.returncode == 0, p.stderr
        signed = self.wait(lambda: self.book_offer("O"), "the order in the book")
        assert_equal(self.listed_at(signed), (N, "OFFER_STATUS_OPEN"))
        self.log.info("LISTED %s:%d at %d", txid, vout, N)

        ftx = self.covfill("O", F1)
        self.mine(1)
        moved = self.wait(lambda: (lambda o: o if o and o["covenant"]["covenant_txid"] == ftx else None)(self.book_offer("O")),
                          "the order re-rested at the fill's output 1")
        assert_equal(moved["covenant"]["covenant_vout"], 1)
        assert_equal(moved["maker_sig"], signed["maker_sig"])
        assert_equal(self.held(ftx, 1), N - F1)
        assert_equal(self.listed_at(moved), (N - F1, "OFFER_STATUS_PARTIAL"))
        self.log.info("PARTIAL fill %s took %d: the order rests at %s:1 holding %d, listed at %d",
                      ftx, F1, ftx, self.held(ftx, 1), N - F1)

        # RESTART, then the maker's signed bytes re-posted naming the remainder.
        self.restart_relay("after the part fill")
        self.adopt(signed, ftx)

        # The refusals, on a relay that holds nothing under the order's key (a
        # re-post of bytes already resting is a no-op, whatever outpoint it names).
        self.restart_relay("for the refusals")
        other = self.derive(3, 1)
        otx, ov = self.fund(other, N - F1)
        self.refused("an outpoint of another program (the same maker's order at 3/1)",
                     self.at(signed, otx, ov), "does not pay the program derived from its terms")
        mtx, mv = self.fund(d, N + 1)
        self.refused("an outpoint of the order's program holding more than the signed amount",
                     self.at(signed, mtx, mv), "holds %d atoms, more than the offer_amount %d its maker signed" % (N + 1, N))
        ltx, lv = self.fund(d, MIN_LOT - 1)
        self.refused("an outpoint of the order's program below min_lot",
                     self.at(signed, ltx, lv), "holds %d atoms, below the order's min_lot %d" % (MIN_LOT - 1, MIN_LOT))
        ctx, cv = self.fund(d, N - F1, blinded=True)
        u = node.gettxout(ctx, cv, True)
        assert "value" not in u and "valuecommitment" in u, u
        self.refused("a confidential outpoint of the order's program",
                     self.at(signed, ctx, cv), "is confidential")
        self.refused("the order's spent funding outpoint", self.at(signed, txid, vout), "is not an unspent output at the tip")
        etx, ev = self.fund(d, MIN_LOT)
        assert_equal(self.post_signed(self.at(signed, etx, ev))[1]["active_amount"], str(MIN_LOT))
        self.log.info("CONTROL an outpoint of the order's program holding exactly min_lot (%d) is listed at %d", MIN_LOT, MIN_LOT)

        # ADOPTED again after a third restart, and traded on.
        self.restart_relay("to clear the control")
        self.adopt(signed, ftx)

        f2 = self.covfill("O", F2)
        self.mine(1)
        again = self.wait(lambda: (lambda o: o if o and o["covenant"]["covenant_txid"] == f2 else None)(self.book_offer("O")),
                          "the re-listed order re-rested again")
        assert_equal(self.held(f2, 1), N - F1 - F2)
        assert_equal(self.listed_at(again)[0], N - F1 - F2)
        self.log.info("PARTIAL fill %s of the re-listed order took %d: listed at %d, the outpoint holds %d",
                      f2, F2, N - F1 - F2, self.held(f2, 1))
        self.bury()

        w = self.covfill("O", 0)
        self.mine(1)
        rec = self.wait(lambda: [x for x in self.market("fills")["fills"] if x["txid"] == w], "the whole fill's record")[0]
        assert_equal((rec["filled"], rec["remainder"]), (N - F1 - F2, 0))
        self.bury()
        self.wait(lambda: self.book_offer("O") is None, "the order removed once its whole fill is final")
        sizes = sorted(int(t["size"]) for t in self.market("trades")["trades"])
        self.log.info("WHOLE fill %s took %d: the order left the book; trades since the restart %s",
                      w, N - F1 - F2, sizes)
        assert_equal(sizes, sorted([F2, N - F1 - F2]))


if __name__ == "__main__":
    SeqObBookRemainderTest().main()
