#!/usr/bin/env python3
# Copyright (c) 2026 The Sequentia developers
# Distributed under the MIT software license.
"""A maker's pool and a clearing round in the relay's book.

On the anchored chain of feature_seqob_pool_round.py, with the production
relay (seqobd, its chain watcher every second) and the maker's posting tool
(seqob-relaycli):

  LISTED   a pool posts its ask (base for quote) and its bid (quote for base),
           each listed at what its own coin holds above its reserve; four round
           orders are listed at what their coins hold
  REFUSED  a pool side naming another template, one whose price is not its
           quote, one whose coin is spent, one with nothing above its reserve;
           a round order whose limit is not its offer's price; a lift of a round
           order and of a pool
  ROUND    the round is laid out from the relay's book (seqob-covenant round)
           and mined: /v1/rounds serves it at its one price with four fills and
           the pool's move; every order is followed to its remainder or out of
           the book, and both sides of the pool to the coins it rests at again
  REORG    the round's block leaves the chain with its Bitcoin anchor: the round
           is undone, the pool and the orders listed again where they rested
           before; mined again, it is recorded again
  FINAL    with two Bitcoin blocks over it the round is final
  KEPT     the relay, given -covenant-book, restarts: both sides of the pool and
           the round order resting at its remainder are listed again by
           themselves where they rest, nobody posting them again, the ask (sold
           down to its reserve) held at 0 as before; restarted again after a
           fill of the pool mined while it was down, it holds both sides, then
           follows them to the coins the fill re-created the pool at
  WITHDRAW the maker withdraws the pool: both sides are held, then delisted
           once the withdrawal is final

Requires the Go toolchain and a configured, built Sequentia node checkout.
"""

import base64
import json
import os
import socket
import subprocess
import time
import urllib.error
import urllib.request

import seqob_env  # noqa: F401
from test_framework.key import ECKey, compute_xonly_pubkey, generate_privkey
from test_framework.messages import tx_from_hex
from test_framework.util import get_auth_cookie, rpc_port

from feature_seqob_pool_round import SeqObPoolRoundTest, internal, FEE

TTL = 3600


def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    p = s.getsockname()[1]
    s.close()
    return p


class SeqObPoolRoundBookTest(SeqObPoolRoundTest):

    def set_test_params(self):
        SeqObPoolRoundTest.set_test_params(self)

    def build(self):
        go = seqob_env.go_bin()
        env = dict(os.environ, PATH=os.path.dirname(go) + os.pathsep + os.environ.get("PATH", ""))
        self.bins = {}
        for name in ("seqobd", "seqob-covenant", "seqob-relaycli"):
            out = os.path.join(self.options.tmpdir, name)
            subprocess.run([go, "build", "-o", out, "./cmd/" + name],
                           cwd=os.path.join(seqob_env.seqdex_dir(), "daemon"), env=env, check=True)
            self.bins[name] = out
        self.cov = self.bins["seqob-covenant"]

    def start_relay(self):
        self.relay_port = free_port()
        self.relay_http = "http://127.0.0.1:%d" % self.relay_port
        user, password = get_auth_cookie(self.nodes[1].datadir, "elementsregtest")
        env = dict(os.environ, SEQOB_LISTEN=":%d" % self.relay_port)
        self.relay_log_path = os.path.join(self.options.tmpdir, "seqobd.log")
        self.relay_log = open(self.relay_log_path, "a")
        self.relay_proc = subprocess.Popen(
            [self.bins["seqobd"], "-min-expiry", "5s", "-node-rpc", "127.0.0.1:%d" % rpc_port(1),
             "-node-rpc-user", user, "-node-rpc-pass", password, "-watch-interval", "1s",
             "-covenant-book", os.path.join(self.options.tmpdir, "covenant-book.jsonl")],
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
            self.relay_log.close()

    def restart_relay(self, while_down=lambda: None):
        """Stops the relay (it writes its covenant book as it stops), runs
        while_down, and starts it again; returns what it logged reading the book
        back, and what while_down returned."""
        self.stop_relay()
        done = while_down()
        start = os.path.getsize(self.relay_log_path)
        self.start_relay()

        def book_line():
            with open(self.relay_log_path) as f:
                f.seek(start)
                for line in f:
                    if "covenant orders listed again" in line:
                        return line.strip()
            return None
        return self.wait(book_line, "the relay reads its covenant book back"), done

    # --- the relay ------------------------------------------------------------------

    def get(self, path):
        with urllib.request.urlopen(self.relay_http + path, timeout=5) as r:
            return json.loads(r.read().decode())

    def post(self, offer, priv=None):
        p = subprocess.run([self.bins["seqob-relaycli"], "post", "--http", self.relay_http, "--priv", priv or self.maker_priv],
                           input=json.dumps(offer), capture_output=True, text=True)
        if p.returncode != 0:
            return None, p.stderr.strip().split("seqob-relaycli: ", 1)[-1]
        return json.loads(p.stdout), None

    def refused(self, what, offer, want):
        st, err = self.post(offer)
        assert st is None and want in (err or ""), (what, st, err)
        self.log.info("REFUSED %s: %s", what, err[:220])

    def size(self, base, quote, offer_id):
        b = self.get("/v1/market/%s/%s/orderbook" % (base, quote))
        for o, st in zip(b.get("offers", []), b.get("statuses", [])):
            if o["offer_id"] == offer_id:
                return int(st.get("active_amount", 0)), o
        return None, None

    def wait(self, predicate, what, timeout=40):
        deadline = time.time() + timeout
        while time.time() < deadline:
            r = predicate()
            if r:
                return r
            time.sleep(0.5)
        raise AssertionError("timed out waiting for: %s" % what)

    def lift(self, offer, amount):
        body = {"offer_id": offer["offer_id"], "maker_pubkey": offer["maker_pubkey"], "take_amount": str(amount)}
        req = urllib.request.Request(self.relay_http + "/v1/lift", data=json.dumps(body).encode(),
                                     headers={"Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=10) as r:
                return r.status, r.read().decode()
        except urllib.error.HTTPError as e:
            return e.code, e.read().decode()

    # --- offers ---------------------------------------------------------------------

    def b64(self, h):
        return base64.b64encode(bytes.fromhex(h)).decode()

    def pool_offer(self, oid, pool, xc, yc, ask, template=None):
        X, Y = self.X, self.Y
        now = int(time.time())
        if ask:
            sells, wants, n, d = X, Y, pool["ask"][0], pool["ask"][1]
            amount = 10_000_000
            want = amount * n // d
        else:
            sells, wants = Y, X
            amount = 9_000_000
            # base per quote is bid_den / bid_num
            want = amount * pool["bid"][1] // pool["bid"][0]
        return {"offer_id": oid, "schema_version": 1, "pair": {"base_asset": sells, "quote_asset": wants},
                "trade_dir": "TRADE_DIR_SELL", "base_amount": str(amount), "offer_amount": str(amount),
                "offer_asset": sells, "want_amount": str(want), "want_asset": wants, "allow_partial": True,
                "min_fill": "1", "created_at_unix": str(now), "expires_at_unix": str(now + TTL),
                "pool": {"base_txid": xc["txid"], "base_vout": xc["vout"], "quote_txid": yc["txid"], "quote_vout": yc["vout"],
                         "base": pool["base"], "quote": pool["quote"],
                         "bid_num": str(pool["bid"][0]), "bid_den": str(pool["bid"][1]),
                         "ask_num": str(pool["ask"][0]), "ask_den": str(pool["ask"][1]),
                         "min_lot": str(pool["min_lot"]), "reserve_base": str(pool["reserve_base"]),
                         "reserve_quote": str(pool["reserve_quote"]), "maker_key": self.b64(pool["maker_key"]),
                         "template_hash": template or self.pool_template}}

    def round_offer(self, oid, t, coin, sells, wants):
        now = int(time.time())
        amount = coin["value"]
        want = -(-amount * t["limit"][0] // t["limit"][1])
        if want * t["limit"][1] != amount * t["limit"][0]:
            raise AssertionError("pick a size the limit divides")
        return {"offer_id": oid, "schema_version": 1, "pair": {"base_asset": sells, "quote_asset": wants},
                "trade_dir": "TRADE_DIR_SELL", "base_amount": str(amount), "offer_amount": str(amount),
                "offer_asset": sells, "want_amount": str(want), "want_asset": wants, "allow_partial": True,
                "min_fill": str(t["min_lot"]), "created_at_unix": str(now), "expires_at_unix": str(now + TTL),
                "round_order": {"covenant_txid": coin["txid"], "covenant_vout": coin["vout"],
                                "asset_a": t["asset_a"], "asset_b": t["asset_b"],
                                "limit_num": str(t["limit"][0]), "limit_den": str(t["limit"][1]),
                                "min_lot": str(t["min_lot"]), "maker_script": self.b64(t["maker_script"]),
                                "expiry_locktime": t["expiry"], "refund_key": self.b64(t["refund_key"]),
                                "clearer": self.b64(t["clearer"]), "template_hash": self.round_template}}

    def bury(self):
        """Two parent blocks over the tip, and a block of ours that anchors them."""
        self.generatetoaddress(self.nodes[0], 2, self.nodes[0].getnewaddress(), sync_fun=self.no_op)
        time.sleep(2)
        self.mine()

    # --- the test -----------------------------------------------------------------------

    def run_test(self):
        parent, node = self.nodes[0], self.nodes[1]
        self.node = node
        self.sizes, self.refusals = {}, {}
        self.build()
        self.generatetoaddress(parent, 5, parent.getnewaddress(), sync_fun=self.no_op)
        time.sleep(2)
        self.miner = node.getnewaddress()
        self.mine(101)
        self.policy = node.dumpassetlabels()["bitcoin"]
        node.sendtoaddress(address=node.getnewaddress(), amount=1000000, fee_asset_label=self.policy)
        self.mine()
        X, Y, Z = [node.issueasset(assetamount=10000000, tokenamount=0, blind=False, fee_asset=self.policy)["asset"]
                   for _ in range(3)]
        self.X, self.Y = X, Y
        self.mine()
        rates = node.getfeeexchangerates()
        rates.update({X: 100000000, Y: 100000000, Z: 100000000})
        node.setfeeexchangerates(rates)
        self.genesis = internal(node.getblockhash(0))
        change = self.fresh_spk()
        fees = self.coins(10, Z, 200_000)
        self.maker_priv = "22" * 32
        k = ECKey()
        k.set(bytes.fromhex(self.maker_priv), True)
        maker_sk, clearer_sk, refund_sk = (generate_privkey() for _ in range(3))
        maker_pk, clearer_pk, refund_pk = (compute_xonly_pubkey(k)[0].hex() for k in (maker_sk, clearer_sk, refund_sk))

        pool = {"base": internal(X), "quote": internal(Y), "bid": [9, 10], "ask": [11, 10], "min_lot": 1000,
                "reserve_base": 1_000_000, "reserve_quote": 1_000_000, "maker_key": maker_pk}
        d = self.go("poolderive", {"pool": pool})
        self.pool_template = d["template_hash"]
        xc, yc = self.fund(d["scriptpubkey"], 1_550_000, X), self.fund(d["scriptpubkey"], 2_065_000, Y)
        h = node.getblockcount()
        maker = self.fresh_spk()

        def order(a, b, limit, expiry):
            return {"asset_a": internal(a), "asset_b": internal(b), "limit": limit, "min_lot": 1000,
                    "maker_script": maker, "expiry": expiry, "refund_key": refund_pk, "clearer": clearer_pk}
        terms = {"A1": order(X, Y, [1, 1], h + 400), "A2": order(X, Y, [105, 100], h + 400),
                 "B1": order(Y, X, [100, 125], h + 400), "B2": order(Y, X, [100, 115], h + 400)}
        # Sizes each order's limit divides, so that its offer states the limit exactly.
        size = {"A1": 4_000_000, "A2": 3_000_000, "B1": 5_000_000, "B2": 9_200_000}
        coin_of = {}
        for name, t in terms.items():
            od = self.go("roundderive", {"order": t})
            self.round_template = od["template_hash"]
            coin_of[name] = self.fund(od["scriptpubkey"], size[name], X if name[0] == "A" else Y)
        spent_y = self.coins(1, Y, 10_000)[0]

        self.start_relay()
        try:
            self.relay_test(X, Y, Z, pool, d, xc, yc, terms, coin_of, fees, change, maker_sk, clearer_sk, spent_y)
        finally:
            self.stop_relay()

    def relay_test(self, X, Y, Z, pool, d, xc, yc, terms, coin_of, fees, change, maker_sk, clearer_sk, spent_y):
        node, parent = self.node, self.nodes[0]
        # ---- REFUSED and LISTED ---------------------------------------------------------
        bad = self.pool_offer("PA", pool, xc, yc, True, template="00" * 32)
        self.refused("a pool side naming another template", bad, "is not seqdex/pool")
        bad = self.pool_offer("PA", pool, xc, yc, True)
        bad["want_amount"] = str(int(bad["want_amount"]) - 1)
        self.refused("a pool ask not at its quote", bad, "does not equal the offer's want/offer")
        self.refused("a pool side at a coin that is not the pool's", self.pool_offer("PA", pool, xc, dict(spent_y), True),
                     "does not pay the pool's script")
        empty = dict(pool, reserve_base=1_550_000)
        de = self.go("poolderive", {"pool": empty})
        ex, ey = self.fund(de["scriptpubkey"], 1_550_000, X), self.fund(de["scriptpubkey"], 1_500_000, Y)
        self.refused("a pool ask with nothing above its reserve", self.pool_offer("PE", empty, ex, ey, True),
                     "holds nothing above its reserve")
        ask, err = self.post(self.pool_offer("PA", pool, xc, yc, True))
        assert err is None, err
        bid, err = self.post(self.pool_offer("PB", pool, xc, yc, False))
        assert err is None, err
        a, ask_offer = self.size(X, Y, "PA")
        b, bid_offer = self.size(Y, X, "PB")
        assert (a, b) == (550_000, 1_065_000), (a, b)
        self.log.info("LISTED the pool's ask at %d X (1,550,000 held, reserve 1,000,000), its bid at %d Y (2,065,000 held)", a, b)
        st, txt = self.lift(ask_offer, 1000)
        assert st >= 400 and "pool" in txt, (st, txt)
        self.log.info("REFUSED a lift of the pool: %s", txt.strip()[:160])

        oids = {}
        for name, t in terms.items():
            sells, wants = (X, Y) if name[0] == "A" else (Y, X)
            off = self.round_offer("R" + name, t, coin_of[name], sells, wants)
            if name == "A2":
                badr = dict(off, want_amount=str(int(off["want_amount"]) + 1))
                self.refused("a round order whose offer is not at its limit", badr, "does not equal the offer's want/offer")
            st, err = self.post(off)
            assert err is None, (name, err)
            oids[name] = (sells, wants, "R" + name)
        for name, (sells, wants, oid) in oids.items():
            s, o = self.size(sells, wants, oid)
            assert s == coin_of[name]["value"], (name, s)
        st, txt = self.lift(self.size(X, Y, "RA1")[1], 1000)
        assert st >= 400 and "clearing round" in txt, (st, txt)
        self.log.info("REFUSED a lift of a round order: %s", txt.strip()[:160])
        self.log.info("LISTED four round orders at what their coins hold")

        # ---- ROUND -------------------------------------------------------------------------
        def book_spec(fc, gap):
            orders = []
            for name in ("A1", "A2", "B1", "B2"):
                sells, wants, oid = oids[name]
                s, o = self.size(sells, wants, oid)
                ro = o["round_order"]
                orders.append({"terms": terms[name], "coin": self.coin(ro["covenant_txid"], ro["covenant_vout"])})
            s, o = self.size(X, Y, "PA")
            pt = o["pool"]
            return {"genesis": self.genesis, "base": internal(X), "quote": internal(Y), "orders": orders,
                    "pools": [{"terms": pool, "base_coin": self.coin(pt["base_txid"], pt["base_vout"]),
                               "quote_coin": self.coin(pt["quote_txid"], pt["quote_vout"])}],
                    "funding": gap + [fc], "change_script": change, "fee_asset": internal(Z), "fee": FEE,
                    "clearer_priv": clearer_sk.hex()}
        gap = [self.coins(1, X, 1_000)[0], self.coins(1, Y, 1_000)[0]]
        rd = self.go("round", book_spec(fees.pop(), gap))
        assert rd["price"] == [23, 20], rd["price"]
        rnd = self.sign(rd["hex"], list(range(6)))
        self.generatetoaddress(parent, 1, parent.getnewaddress(), sync_fun=self.no_op)
        time.sleep(2)
        rtx = self.send(rnd, "round/from_the_relays_book")

        def round_rec(state):
            for r in self.get("/v1/rounds").get("rounds", []):
                if (r["txid"] == rtx and r["state"] == state and len(r["fills"]) == 4 and len(r["pool_moves"]) >= 1
                        and all(f["state"] == state for f in r["fills"]) and all(m["state"] == state for m in r["pool_moves"])):
                    return r
            return None
        r = self.wait(lambda: round_rec("confirmed"), "the relay records the round")
        assert (r["price"]["n"], r["price"]["d"]) == (23, 20), r["price"]
        assert r["clearer"] == compute_xonly_pubkey(clearer_sk)[0].hex(), r["clearer"]
        mv = r["pool_moves"][0]
        assert mv["sell"] and mv["base_after"] == pool["reserve_base"] and mv["round_price"]["n"] == 23, mv
        self.log.info("ROUND /v1/rounds: %s at %d/%d, %d fills (%s), the pool sold %d X and rests with %d X, %d Y",
                      rtx[:16], r["price"]["n"], r["price"]["d"], len(r["fills"]),
                      ", ".join("%s %d->%d" % (f["offer_id"], f["filled"], f["credit"]["value"]) for f in r["fills"]),
                      mv["base"], mv["base_after"], mv["quote_after"])
        # The pool follows to 2j and 2j+1 (j = 4), its ask at 0 (it sold down to its
        # reserve), its bid larger; B2 rests at output 7; the whole fills are held.
        self.wait(lambda: self.size(X, Y, "PA")[1]["pool"]["base_txid"] == rtx and self.size(Y, X, "PB")[1]["pool"]["base_txid"] == rtx
                  and self.size(Y, X, "RB2")[1]["round_order"]["covenant_txid"] == rtx, "both sides of the pool and B2 re-pointed")
        a, ao = self.size(X, Y, "PA")
        b, bo = self.size(Y, X, "PB")
        assert (ao["pool"]["base_vout"], ao["pool"]["quote_vout"], a) == (8, 9, 0), (ao["pool"], a)
        assert b == mv["quote_after"] - pool["reserve_quote"], (b, mv)
        s2, o2 = self.size(Y, X, "RB2")
        assert (o2["round_order"]["covenant_txid"], o2["round_order"]["covenant_vout"]) == (rtx, 7), o2["round_order"]
        self.log.info("ROUND the pool's sides re-pointed to %s:8/9 (ask %d, bid %d); B2 rests at %s:7 with %d Y", rtx[:16], a, b, rtx[:16], s2)

        # ---- REORG -------------------------------------------------------------------------
        self.orphan_anchor_of(rtx)
        r = self.wait(lambda: round_rec("undone"), "the relay undoes the round")
        # The pool and B2 point at the coins they rested at before the round; the
        # round is in the mempool again, spending them, so both are held at 0 until
        # it settles.
        self.wait(lambda: all(self.size(m, q, o)[1]["pool"]["base_txid"] == xc["txid"] and self.size(m, q, o)[0] == 0
                              for m, q, o in ((X, Y, "PA"), (Y, X, "PB"))),
                  "both sides back at the coins the pool rested at before, held while the round is unconfirmed")
        assert self.size(Y, X, "RB2")[1]["round_order"]["covenant_txid"] == coin_of["B2"]["txid"]
        assert rtx in node.getrawmempool()
        assert all(f["state"] == "undone" for f in r["fills"]), [(f["offer_id"], f["state"], f.get("reason")) for f in r["fills"]]
        self.log.info("REORG the round undone (%s); the pool back at %s:%d, B2 back at its funding, both held at 0 while the round waits in the mempool",
                      "; ".join(sorted(set(f.get("reason", "") for f in r["fills"]))), xc["txid"][:16], xc["vout"])
        time.sleep(2)
        self.mine()
        r = self.wait(lambda: round_rec("confirmed"), "the round recorded again")
        self.wait(lambda: self.size(X, Y, "PA")[1]["pool"]["base_txid"] == rtx and self.size(Y, X, "PB")[1]["pool"]["base_txid"] == rtx,
                  "both sides re-pointed again")
        self.log.info("REORG mined again: the round recorded again, the pool at %s:8/9", rtx[:16])

        # ---- FINAL -------------------------------------------------------------------------
        self.bury()
        r = self.wait(lambda: round_rec("final"), "the round final")
        self.log.info("FINAL the round is final at anchor depth %d", r["anchor_depth"])

        # ---- KEPT ----------------------------------------------------------------------------
        sides = ((X, Y, "PA"), (Y, X, "PB"), (Y, X, "RB2"))

        def where(o):
            t = o.get("pool") or o["round_order"]
            if "pool" in o:
                return (t["base_txid"], t["base_vout"], t["quote_txid"], t["quote_vout"])
            return (t["covenant_txid"], t["covenant_vout"])
        before = {oid: (self.size(m, q, oid)[0], where(self.size(m, q, oid)[1])) for m, q, oid in sides}
        assert before["PA"][0] == 0 and before["PB"][0] > 0, before
        line, _ = self.restart_relay()
        assert "2 covenant orders listed again, 1 held for the watcher to follow a spend, 0 dropped" in line, line

        def back_as_before():
            now = {}
            for m, q, oid in sides:
                n, o = self.size(m, q, oid)
                if o is None:
                    return None
                now[oid] = (n, where(o))
            return now == before and now
        self.wait(back_as_before, "the pool's sides and B2 listed again as they were, nobody posting them")
        self.log.info("KEPT restarted with -covenant-book (%s): the pool's ask at %d and bid at %d at %s:8/9, B2 at %d at %s:7, as before",
                      line.split("covenant book ", 1)[1].split(": ", 1)[1], before["PA"][0], before["PB"][0], rtx[:16],
                      before["RB2"][0], rtx[:16])
        # A taker sells the pool 50,000 X at its bid while the relay is down.
        pay_x, fc = self.coins(1, X, 200_000)[0], fees.pop()
        xc1, yc1 = self.coin(rtx, 8), self.coin(rtx, 9)

        def buy():
            f = self.go("poolfill", {"pool": pool, "base_coin": xc1, "quote_coin": yc1, "sell": False, "base": 50_000,
                                     "inputs": [pay_x, fc],
                                     "outputs": [self.out(Y, 45_000, self.fresh_spk()), self.out(X, pay_x["value"] - 50_000, change),
                                                 self.out(Z, fc["value"] - FEE, change), self.out(Z, FEE, "")]})
            assert f["quote"] == 45_000, f
            return self.send(self.sign(f["hex"], [0, 1]), "pool/bought_at_its_bid_while_the_relay_was_down")
        line, ftx = self.restart_relay(buy)
        assert "1 covenant orders listed again, 2 held for the watcher to follow a spend, 0 dropped" in line, line
        want = {"PA": (50_000, (ftx, 0, ftx, 1)), "PB": (before["PB"][0] - 45_000, (ftx, 0, ftx, 1)), "RB2": before["RB2"]}
        self.wait(lambda: {oid: (self.size(m, q, oid)[0], where(self.size(m, q, oid)[1])) for m, q, oid in sides} == want,
                  "both sides followed to the coins the fill re-created the pool at")
        mv = next(m for m in self.get("/v1/pools/moves")["moves"] if m["txid"] == ftx)
        assert not mv["sell"] and mv["base"] == 50_000 and mv["base_after"] == pool["reserve_base"] + 50_000, mv
        self.log.info("KEPT the pool bought 50,000 X while the relay was down (%s); restarted (%s), "
                      "both sides followed to %s:0/1: ask %d, bid %d; /v1/pools/moves records the fill",
                      ftx[:16], line.split("covenant book ", 1)[1].split(": ", 1)[1], ftx[:16], want["PA"][0], want["PB"][0])

        # ---- WITHDRAW ------------------------------------------------------------------------
        xc2, yc2 = self.coin(ftx, 0), self.coin(ftx, 1)
        fc = fees.pop()
        w = self.go("poolwithdraw", {"pool": pool, "genesis": self.genesis, "coins": [xc2, yc2], "inputs": [fc],
                                     "outputs": [self.out(X, xc2["value"], self.fresh_spk()),
                                                 self.out(Y, yc2["value"], self.fresh_spk()),
                                                 self.out(Z, fc["value"] - FEE, change), self.out(Z, FEE, "")],
                                     "maker_priv": maker_sk.hex()})
        wtx = self.send(self.sign(w["hex"], [0, 1]), "pool/withdrawn_by_its_maker")
        self.wait(lambda: self.size(Y, X, "PB")[0] == 0, "the pool's bid held at 0 while the withdrawal settles")
        self.log.info("WITHDRAW %s: both sides held at 0", wtx[:16])
        self.bury()
        self.wait(lambda: self.size(X, Y, "PA")[0] is None and self.size(Y, X, "PB")[0] is None, "both sides delisted")
        moves = self.get("/v1/pools/moves")["moves"]
        assert any(m["txid"] == wtx and m.get("withdrawn") for m in moves), moves
        self.log.info("WITHDRAW final: both sides delisted; /v1/pools/moves records %d moves", len(moves))


if __name__ == "__main__":
    SeqObPoolRoundBookTest().main()
