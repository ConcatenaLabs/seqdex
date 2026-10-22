#!/usr/bin/env python3
# Copyright (c) 2026 The Sequentia developers
# Distributed under the MIT software license.
"""A maker's pool and a clearing round, built by this repository's Go.

On an anchored regtest chain with Simplicity active from genesis (node 0
stands in for Bitcoin, node 1 anchors to it, -validateanchor, -par=1), every
spend below is laid out and witnessed by `seqob-covenant` (pkg/covenant
SimPool, RoundOrder, ClearPrice, Allocate, BuildRound, SimSigAllHash), the
node's wallet signing only its own coins:

  POOL    a pool (seqdex/pool) funded by its maker with a coin of X and a coin
          of Y; a taker buys X from it at its ask and sells X to it at its bid;
          a sell below its reserve, one under its ask, a buy above its bid and
          a fill padded with an annex are forced into a block and refused,
          each beside its accepted control
  ROUND   four round orders (seqdex/round-order) of one clearer, two selling X
          and two selling Y, and the pool, cleared by ClearPrice at one price
          read from the round's last output: every order credited exactly at
          that price, one in part, the pool selling down to its reserve; a
          credit at the order's own limit, a round signed by another key and
          one whose clearing output is not last are forced into a block and
          refused beside the round itself
  REORG   the round's block dropped with its orphaned Bitcoin anchor: the round
          is back in the mempool and mined again
  KEYS    the maker withdraws the pool, each coin signed over its Simplicity
          sig_all_hash as Go computes it, a withdrawal by another key refused;
          a round order's remainder refunded by its refund key after expiry

Every transaction pays its fee in an issued asset; none touches the policy
asset. Requires the Go toolchain and a configured, built Sequentia node
checkout; see seqob_env.py.
"""

import json
import os
import subprocess
import time
from decimal import Decimal

import seqob_env  # noqa: F401  (puts the node's test_framework on sys.path)
from test_framework.test_framework import BitcoinTestFramework, SkipTest
from test_framework.util import get_auth_cookie, get_datadir_path, p2p_port, rpc_port
from test_framework.address import program_to_witness
from test_framework.key import compute_xonly_pubkey, generate_privkey
from test_framework.messages import COIN, tx_from_hex, CTxInWitness

JET = "Assertion failed inside jet"
FEE = 30_000


def internal(display):
    return bytes.fromhex(display)[::-1].hex()


def ceil_div(a, b):
    return -(-a // b)


class SeqObPoolRoundTest(BitcoinTestFramework):

    def set_test_params(self):
        self.setup_clean_chain = True
        self.num_nodes = 2

    def skip_test_if_missing_module(self):
        self.skip_if_no_wallet()
        if not os.path.exists(seqob_env.go_bin()):
            raise SkipTest("Go toolchain not found at %s (set GO_BIN)" % seqob_env.go_bin())

    def setup_network(self, split=False):
        self.add_nodes(1, [[
            "-port=%d" % p2p_port(0), "-rpcport=%d" % rpc_port(0), "-validatepegin=0",
            "-initialfreecoins=0", "-con_blocksubsidy=5000000000", "-anyonecanspendaremine=1",
            "-signblockscript=51",
        ]], chain=["elementsregtest"])
        self.start_node(0)
        u, p = get_auth_cookie(get_datadir_path(self.options.tmpdir, 0), "elementsregtest")
        self.add_nodes(1, [[
            "-port=%d" % p2p_port(1), "-rpcport=%d" % rpc_port(1), "-validatepegin=0",
            "-initialfreecoins=2100000000000000", "-anyonecanspendaremine=1", "-con_connect_genesis_outputs=1",
            "-signblockscript=51", "-evbparams=simplicity:-1:::",
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
        self.nodes[1].rescanblockchain()

    # --- Go ---------------------------------------------------------------------

    def build(self):
        go = seqob_env.go_bin()
        out = os.path.join(self.options.tmpdir, "seqob-covenant")
        env = dict(os.environ, PATH=os.path.dirname(go) + os.pathsep + os.environ.get("PATH", ""))
        subprocess.run([go, "build", "-o", out, "./cmd/seqob-covenant"],
                       cwd=os.path.join(seqob_env.seqdex_dir(), "daemon"), env=env, check=True)
        self.cov = out

    def go(self, cmd, spec, fail=False):
        p = subprocess.run([self.cov, cmd], input=json.dumps(spec), capture_output=True, text=True)
        if fail:
            assert p.returncode != 0, p.stdout
            return p.stderr.strip()
        assert p.returncode == 0, "%s: %s" % (cmd, p.stderr)
        return json.loads(p.stdout)

    # --- chain --------------------------------------------------------------------

    def mine(self, n=1):
        self.generatetoaddress(self.node, n, self.miner, sync_fun=self.no_op)

    def spk(self, addr):
        return self.node.getaddressinfo(addr)["scriptPubKey"]

    def fresh_spk(self):
        return self.spk(self.node.getnewaddress("", "bech32"))

    def coins(self, n, asset, atoms):
        """n wallet coins of `asset`, in one transaction and one block, locked so
        the wallet's own sends leave them."""
        node = self.node
        addrs = [node.getnewaddress("", "bech32") for _ in range(n)]
        txid = node.sendmany(dummy="", amounts={a: Decimal(atoms) / COIN for a in addrs},
                             output_assets={a: asset for a in addrs}, fee_asset=self.policy)
        self.mine()
        tx = node.getrawtransaction(txid, True)
        out = []
        for a in addrs:
            s = self.spk(a)
            for o in tx["vout"]:
                if o["scriptPubKey"]["hex"] == s:
                    out.append(self.coin(txid, o["n"]))
        node.lockunspent(False, [{"txid": c["txid"], "vout": c["vout"]} for c in out])
        return out

    def fund(self, spk_hex, atoms, asset):
        addr = program_to_witness(1, bytes.fromhex(spk_hex)[2:])
        txid = self.node.sendtoaddress(address=addr, amount=Decimal(atoms) / COIN, assetlabel=asset,
                                       fee_asset_label=self.policy)
        self.mine()
        for o in self.node.getrawtransaction(txid, True)["vout"]:
            if o["scriptPubKey"]["hex"] == spk_hex:
                return self.coin(txid, o["n"])
        raise AssertionError("not funded")

    def coin(self, txid, vout):
        o = self.node.getrawtransaction(txid, True)["vout"][vout]
        return {"txid": txid, "vout": vout, "asset": internal(o["asset"]),
                "value": int(Decimal(str(o["value"])) * COIN), "script": o["scriptPubKey"]["hex"]}

    def out(self, asset_display, value, script):
        return {"asset": internal(asset_display), "value": value, "script": script}

    def sign(self, hexstr, keep):
        """The wallet signs its own inputs; the inputs in `keep` keep the
        witnesses Go gave them."""
        tx = tx_from_hex(hexstr)
        saved = {i: list(tx.wit.vtxinwit[i].scriptWitness.stack) for i in keep}
        for i in keep:
            tx.wit.vtxinwit[i] = CTxInWitness()
        r = self.node.signrawtransactionwithwallet(tx.serialize().hex())
        t2 = tx_from_hex(r["hex"])
        for i, st in saved.items():
            t2.wit.vtxinwit[i].scriptWitness.stack = st
        return t2.serialize().hex()

    def accepted(self, hexstr, label):
        r = self.node.testmempoolaccept([hexstr])[0]
        assert r["allowed"], (label, r)
        return r

    def send(self, hexstr, label):
        self.accepted(hexstr, label)
        txid = self.node.sendrawtransaction(hexstr)
        self.mine()
        d = self.node.getrawtransaction(txid, True)
        assert d["confirmations"] >= 1
        self.log.info("MINED %-44s vsize %d", label, d["vsize"])
        self.sizes[label] = d["vsize"]
        return txid

    def refused(self, hexstr, label, control, expect=JET):
        """The mempool refuses `hexstr`, and so does a block forced with
        generateblock, for the program's failed assertion (`expect`, matched
        whole: "Assertion failed" is not "Assertion failed inside jet");
        `control`, which differs from it only in the property, is accepted."""
        self.accepted(control, label + " (control)")
        r = self.node.testmempoolaccept([hexstr])[0]
        assert not r["allowed"], label
        h0 = self.node.getblockcount()
        try:
            self.node.generateblock(self.miner, [hexstr], invalid_call=False)
            raise AssertionError("%s was mined" % label)
        except Exception as e:  # JSONRPCException
            msg = str(e)
        assert self.node.getblockcount() == h0
        assert ("(%s)" % expect) in msg, (label, msg)
        self.log.info("REFUSED %-42s block: %s (control accepted)", label, msg[:110])
        self.refusals[label] = msg

    def orphan_anchor_of(self, txid):
        node, parent = self.node, self.nodes[0]
        blk = node.getrawtransaction(txid, True)["blockhash"]
        hdr = node.getblockheader(blk)
        tip = parent.getblockcount()
        parent.invalidateblock(hdr["anchorhash"])
        self.generatetoaddress(parent, tip - hdr["anchorheight"] + 2, parent.getnewaddress(), sync_fun=self.no_op)
        for _ in range(180):
            if node.getrawtransaction(txid, True).get("confirmations", 0) == 0:
                return hdr
            time.sleep(0.5)
        raise AssertionError("the round's block outlived its anchor")

    # --- the test -------------------------------------------------------------------

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
        assert node.getanchorstatus()["anchorstatus"] == "ok"
        node.sendtoaddress(address=node.getnewaddress(), amount=1000000, fee_asset_label=self.policy)
        self.mine()
        X, Y, Z = [node.issueasset(assetamount=10000000, tokenamount=0, blind=False, fee_asset=self.policy)["asset"]
                   for _ in range(3)]
        self.mine()
        rates = node.getfeeexchangerates()
        rates.update({X: 100000000, Y: 100000000, Z: 100000000})
        node.setfeeexchangerates(rates)
        self.genesis = internal(node.getblockhash(0))
        change = self.fresh_spk()
        fees = self.coins(12, Z, 200_000)

        # ---- POOL ---------------------------------------------------------------------
        maker_sk, other_sk, clearer_sk, refund_sk = (generate_privkey() for _ in range(4))
        maker_pk, clearer_pk, refund_pk = (compute_xonly_pubkey(k)[0].hex() for k in (maker_sk, clearer_sk, refund_sk))
        pool = {"base": internal(X), "quote": internal(Y), "bid": [9, 10], "ask": [11, 10], "min_lot": 1000,
                "reserve_base": 1_000_000, "reserve_quote": 1_000_000, "maker_key": maker_pk}
        d = self.go("poolderive", {"pool": pool})
        assert node.getaddressinfo(d["address"]["elementsregtest"])["scriptPubKey"] == d["scriptpubkey"]
        pool_spk = d["scriptpubkey"]
        xc, yc = self.fund(pool_spk, 1_600_000, X), self.fund(pool_spk, 2_000_000, Y)
        self.log.info("pool %s funded by its maker: %d X, %d Y", d["address"]["elementsregtest"], xc["value"], yc["value"])
        pay_y, pay_x = self.coins(1, Y, 5_000_000)[0], self.coins(1, X, 5_000_000)[0]

        def fill(sell, base, pay, fc, quote=None, unchecked=False):
            spec = {"pool": pool, "base_coin": xc, "quote_coin": yc, "sell": sell, "base": base,
                    "inputs": [pay, fc], "unchecked": unchecked}
            if quote is not None:
                spec["quote"] = quote
            want = quote
            if want is None:
                want = ceil_div(base * 11, 10) if sell else base * 9 // 10
            got, paid = (X, Y) if sell else (Y, X)
            spec["outputs"] = [self.out(got, base if sell else want, self.fresh_spk()),
                               self.out(paid, pay["value"] - (want if sell else base), change),
                               self.out(Z, fc["value"] - FEE, change), self.out(Z, FEE, "")]
            r = self.go("poolfill", spec)
            return self.sign(r["hex"], [0, 1]), r

        fc = fees.pop()
        sell, r = fill(True, 100_000, pay_y, fc)
        assert r["quote"] == 110_000
        below, _ = fill(True, 600_001, pay_y, fc, quote=ceil_div(600_001 * 11, 10), unchecked=True)
        self.refused(below, "pool/sell_below_its_reserve", sell)
        cheap, _ = fill(True, 100_000, pay_y, fc, quote=109_999, unchecked=True)
        self.refused(cheap, "pool/sell_one_atom_under_its_ask", sell)
        # A relay pads the fill with an annex on the pool's base coin: nothing
        # in a fill signs it, and the pool refuses it.
        t = tx_from_hex(sell)
        t.wit.vtxinwit[0].scriptWitness.stack.append(b"\x50" + b"\xab" * 999)
        self.refused(t.serialize().hex(), "pool/sell_padded_with_an_annex_by_its_relay", sell, expect="Assertion failed")
        txid = self.send(sell, "pool/sell_at_its_ask")
        xc, yc = self.coin(txid, 0), self.coin(txid, 1)
        pay_y = self.coin(txid, 3)
        node.lockunspent(False, [{"txid": pay_y["txid"], "vout": pay_y["vout"]}])
        fc = fees.pop()
        buy, r = fill(False, 50_000, pay_x, fc)
        assert r["quote"] == 45_000
        dear, _ = fill(False, 50_000, pay_x, fc, quote=45_001, unchecked=True)
        self.refused(dear, "pool/buy_one_atom_over_its_bid", buy)
        txid = self.send(buy, "pool/buy_at_its_bid")
        xc, yc = self.coin(txid, 0), self.coin(txid, 1)
        assert (xc["value"], yc["value"]) == (1_550_000, 2_065_000), (xc, yc)

        # ---- ROUND ------------------------------------------------------------------------
        h = node.getblockcount()
        maker = self.fresh_spk()

        def order(a, b, limit, expiry):
            return {"asset_a": internal(a), "asset_b": internal(b), "limit": limit, "min_lot": 1000,
                    "maker_script": maker, "expiry": expiry, "refund_key": refund_pk, "clearer": clearer_pk}
        terms = {"A1": order(X, Y, [1, 1], h + 400), "A2": order(X, Y, [105, 100], h + 400),
                 # A bid's limit is X per Y: at most 1.25 and 1.15 Y per X.
                 "B1": order(Y, X, [100, 125], h + 400), "B2": order(Y, X, [100, 115], h + 60)}
        size = {"A1": 4_000_000, "A2": 3_000_000, "B1": 5_000_000, "B2": 9_000_000}
        coin_of = {}
        for name, t in terms.items():
            od = self.go("roundderive", {"order": t})
            assert node.getaddressinfo(od["address"]["elementsregtest"])["scriptPubKey"] == od["scriptpubkey"]
            coin_of[name] = self.fund(od["scriptpubkey"], size[name], X if name[0] == "A" else Y)
        gap = [self.coins(1, X, 1_000)[0], self.coins(1, Y, 1_000)[0]]
        fc = fees.pop()
        names = ["A1", "A2", "B1", "B2"]
        spec = {"genesis": self.genesis, "base": internal(X), "quote": internal(Y),
                "orders": [{"terms": terms[n], "coin": coin_of[n]} for n in names],
                "pools": [{"terms": pool, "base_coin": xc, "quote_coin": yc}],
                "funding": gap + [fc], "change_script": change, "fee_asset": internal(Z), "fee": FEE,
                "clearer_priv": clearer_sk.hex()}
        rd = self.go("round", spec)
        self.log.info("ClearPrice: %s; fills %s; moves %s", rd["price"], rd["fills"], rd["moves"])
        # The asks hold 7,000,000 X at limits up to 1.05 and the pool sells
        # 550,000 X above its reserve at 1.10; the bids pay up to 1.25 and 1.15.
        # All 7,550,000 X change hands at every price from 1.10 to 1.15, and
        # 1.15, B2's limit, leaves the least unmatched: every ask and the pool
        # sell all they list, B1 fills whole and B2 in part.
        assert rd["price"] == [23, 20], rd["price"]
        N, D = rd["price"]
        fills = {names[f["K"]]: f for f in rd["fills"]}
        assert len(fills) == 4 and fills["B2"]["Partial"] and not any(fills[n]["Partial"] for n in ("A1", "A2", "B1"))
        for n, f in fills.items():
            num, den = (N, D) if n[0] == "A" else (D, N)
            assert f["Credited"] == ceil_div(f["Filled"] * num, den), (n, f)
        mv = rd["moves"][0]
        assert mv["Sell"] and mv["X1"] == pool["reserve_base"], mv
        rnd = self.sign(rd["hex"], list(range(len(names) + 2)))
        coins = rd["coins"]

        def resign(hexstr, sk=clearer_sk):
            r = self.go("roundsign", {"tx": hexstr, "coins": coins, "genesis": self.genesis, "price": rd["price"],
                                      "base": internal(X), "quote": internal(Y), "clearer_priv": sk.hex(),
                                      "orders": [{"k": f["K"], "terms": terms[names[f["K"]]], "partial": f["Partial"]}
                                                 for f in rd["fills"]]})
            return self.sign(r["hex"], list(range(len(names) + 2)))

        # A1 credited at its own limit, 1 Y per X, the clearer keeping the rest.
        t = tx_from_hex(rd["hex"])
        a1 = fills["A1"]
        short = a1["Credited"] - a1["Filled"]
        t.vout[0].nValue.setToAmount(a1["Filled"])
        for o in t.vout:
            if o.scriptPubKey.hex() == change and o.nAsset.vchCommitment[1:].hex() == internal(Y):
                o.nValue.setToAmount(o.nValue.getAmount() + short)
                break
        else:
            raise AssertionError("no Y change to take the difference")
        self.refused(resign(t.serialize().hex()), "round/credit_at_the_orders_own_limit", rnd)
        self.refused(resign(rd["hex"], other_sk), "round/signed_by_another_key", rnd)
        t = tx_from_hex(rd["hex"])
        t.vout[-1], t.vout[-2] = t.vout[-2], t.vout[-1]
        t.wit.vtxoutwit[-1], t.wit.vtxoutwit[-2] = t.wit.vtxoutwit[-2], t.wit.vtxoutwit[-1]
        self.refused(resign(t.serialize().hex()), "round/clearing_output_not_last", rnd)
        # A parent block of its own under the round, so that orphaning it takes
        # the round's block and no other.
        self.generatetoaddress(parent, 1, parent.getnewaddress(), sync_fun=self.no_op)
        time.sleep(2)
        rtx = self.send(rnd, "round/four_orders_and_the_pool_at_one_price")
        last = node.getrawtransaction(rtx, True)["vout"][-1]["scriptPubKey"]
        self.log.info("published: %s", last["asm"])

        # ---- REORG --------------------------------------------------------------------------
        hdr = self.orphan_anchor_of(rtx)
        assert rtx in node.getrawmempool()
        self.log.info("REORG the round %s left the chain with its anchor %s (parent height %d) and is in the mempool again",
                      rtx[:16], hdr["anchorhash"][:16], hdr["anchorheight"])
        time.sleep(2)
        self.mine()
        again = node.getrawtransaction(rtx, True)
        assert again["confirmations"] >= 1
        self.log.info("REORG mined again in %s, anchored to %s", again["blockhash"][:16],
                      node.getblockheader(again["blockhash"])["anchorhash"][:16])

        # ---- KEYS ---------------------------------------------------------------------------
        k_pool = len(names)
        xc, yc = self.coin(rtx, 2 * k_pool), self.coin(rtx, 2 * k_pool + 1)
        assert xc["value"] == pool["reserve_base"]
        fc = fees.pop()

        def withdraw(sk):
            r = self.go("poolwithdraw", {"pool": pool, "genesis": self.genesis, "coins": [xc, yc], "inputs": [fc],
                                         "outputs": [self.out(X, xc["value"], self.fresh_spk()),
                                                     self.out(Y, yc["value"], self.fresh_spk()),
                                                     self.out(Z, fc["value"] - FEE, change), self.out(Z, FEE, "")],
                                         "maker_priv": sk.hex()})
            return self.sign(r["hex"], [0, 1])
        w = withdraw(maker_sk)
        self.refused(withdraw(other_sk), "pool/withdrawn_by_another_key", w)
        self.send(w, "pool/withdrawn_by_its_maker")

        b2 = terms["B2"]
        rest = self.coin(rtx, 2 * names.index("B2") + 1)
        assert rest["script"] == self.go("roundderive", {"order": b2})["scriptpubkey"]
        self.mine(max(0, b2["expiry"] - node.getblockcount()))
        fc = fees.pop()
        rf = self.go("roundrefund", {"order": b2, "genesis": self.genesis, "coin": rest, "inputs": [fc],
                                     "locktime": b2["expiry"], "refund_priv": refund_sk.hex(),
                                     "outputs": [self.out(Y, rest["value"], self.fresh_spk()),
                                                 self.out(Z, fc["value"] - FEE, change), self.out(Z, FEE, "")]})
        self.send(self.sign(rf["hex"], [0]), "round_order/refunded_by_its_key")
        self.log.info("sizes: %s", json.dumps(self.sizes))
        self.log.info("refusals: %d, each in a forced block for the program's failed assertion", len(self.refusals))


if __name__ == "__main__":
    SeqObPoolRoundTest().main()
