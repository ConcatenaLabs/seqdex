#!/usr/bin/env python3
# Copyright (c) 2026 The Sequentia developers
# Distributed under the MIT software license.
"""The two versions of the Simplicity order, and an annex on a fill.

A fill of seqdex/order signs nothing, and the node relays an annex of up to
100,000 bytes on a Simplicity leaf spend, so whoever relays a fill could attach
one: the fill grows, its fee rate falls to the relay floor, and the mempool
keeps that copy when the honest one arrives (both share a txid). Version 2 of
the template refuses a spend that carries an annex; version 1, which orders
resting from before version 2 are under, takes one.

On an anchored regtest chain (node 1 stands in for Bitcoin, node 0 anchors to
it, -validateanchor=1, -par=1, Simplicity active from genesis), with every
witness built by SeqDEX's Go (seqob-covenant simwitness) and every address
derived by it (simderive):

  ADDRESS for each version, simderive's address is the script the node reads,
          and the two versions give one order's terms two addresses
  REJECT  version 2: a partial fill with an annex of 3 and of 1,000 bytes and
          a zero annex of 1,000 bytes on the order's input, refused by the
          mempool and FORCED INTO A BLOCK, refused there for the program's
          failure (Assertion failed), beside the same fill without the annex,
          which is mined. The mempool's reason is the program's on a node
          before Sequentia 25.2.4, and the annex itself on one from it, which
          relays an annex only when it is zeros and as large as the program's
          cost needs
  BEFORE  version 1: the same fill with a 1,000-byte annex is mined in a
          block (forced, where relay refuses the annex): the program takes
          an annex nobody signed. On a node before 25.2.4 the mempool also
          takes it with a 1,000- and a 10,000-byte annex
  PASS    the version 1 order's remainder filled whole, mined: an order under
          version 1 still fills

Every fill pays its fee in an issued asset; none touches the policy asset.
Requires the Go toolchain and a configured, built Sequentia node checkout; see
seqob_env.py.
"""

import seqob_env  # noqa: F401  (puts the node's test_framework on sys.path)
from decimal import Decimal

from test_framework.address import program_to_witness
from test_framework.key import compute_xonly_pubkey, generate_privkey
from test_framework.messages import COIN, CTxOut, CTxOutAsset, CTxOutValue, tx_from_hex
from test_framework.util import assert_equal, get_auth_cookie, get_datadir_path, p2p_port, rpc_port

from feature_seqob_simplicity_order import FEE, SeqObSimplicityOrderTest, ceil_price

LOCKED = 20_000_000_000_000
NUM, DEN = 9_000_000_000_007, 10_000_000_000_003
ANNEX_REFUSED = "(Assertion failed)"
# A node from Sequentia 25.2.4 refuses an annex the program's cost does not need
# at relay, before the program runs.
RELAY_ANNEX = "Simplicity annex"


class SeqObSimplicityAnnexTest(SeqObSimplicityOrderTest):

    def set_test_params(self):
        self.setup_clean_chain = True
        self.num_nodes = 2

    def setup_network(self, split=False):
        chain = "elementsregtest"
        # node 1: the parent chain, standing in for Bitcoin; node 0: the
        # anchored chain the orders live on, started once the parent is up.
        self.add_nodes(2, [[], [
            "-port=%d" % p2p_port(1), "-rpcport=%d" % rpc_port(1), "-validatepegin=0",
            "-initialfreecoins=0", "-con_blocksubsidy=5000000000", "-anyonecanspendaremine=1",
            "-signblockscript=51",
        ]], chain=[chain, chain])
        self.start_node(1)
        u, p = get_auth_cookie(get_datadir_path(self.options.tmpdir, 1), chain)
        self.start_node(0, extra_args=[
            "-port=%d" % p2p_port(0), "-rpcport=%d" % rpc_port(0), "-validatepegin=0",
            "-initialfreecoins=2100000000000000", "-anyonecanspendaremine=1", "-signblockscript=51",
            "-con_bitcoin_anchor=1", "-validateanchor=1", "-anchorpollinterval=1",
            "-mainchainrpchost=127.0.0.1", "-mainchainrpcport=%d" % rpc_port(1),
            "-mainchainrpcuser=%s" % u, "-mainchainrpcpassword=%s" % p,
            "-parentgenesisblockhash=%s" % self.nodes[1].getblockhash(0),
            "-blindedaddresses=0", "-con_default_blinded_addresses=0", "-con_any_asset_fees=1",
            "-maxtxfee=100.0", "-txindex=1", "-par=1",
            # Simplicity active from genesis; simplicity:0::: would wait to height 384.
            "-evbparams=simplicity:-1:::",
        ])
        for n in self.nodes:
            n.createwallet(wallet_name=self.default_wallet_name)
        self.nodes[0].rescanblockchain()  # the genesis's free coins (-anyonecanspendaremine)

    # The two chains are not connected: never wait for them to agree.
    def generate(self, node, n, **kw):
        return self.generatetoaddress(node, n, node.getnewaddress(), sync_fun=self.no_op)

    @staticmethod
    def sim_flags(o):
        return SeqObSimplicityOrderTest.sim_flags(o) + ["-template-version", str(o["version"])]

    def coin(self, spk, atoms, asset):
        """Pay atoms of asset to spk, mine it, and return (txid, vout, atoms)."""
        node = self.nodes[0]
        ver = 0 if spk[0] == 0 else spk[0] - 0x50
        txid = node.sendtoaddress(address=program_to_witness(ver, spk[2:]), amount=Decimal(atoms) / COIN,
                                  assetlabel=asset, fee_asset_label=self.policy)
        self.generate(node, 1)
        for o in node.getrawtransaction(txid, True)["vout"]:
            if o["scriptPubKey"]["hex"] == spk.hex():
                node.lockunspent(False, [{"txid": txid, "vout": o["n"]}])
                return (txid, o["n"], atoms)
        raise AssertionError("no output to %s" % spk.hex())

    @staticmethod
    def annexed(tx_hex, k, n, fill=b"\xab"):
        """tx_hex with an annex of n bytes (0x50, then fill) on input k."""
        t = tx_from_hex(tx_hex)
        t.wit.vtxinwit[k].scriptWitness.stack.append(b"\x50" + fill * (n - 1))
        return t.serialize().hex()

    def refuse_both(self, label, tx_hex):
        """Refused by the mempool (by the program, or for the annex itself by a
        node from 25.2.4) and by a block forced with generateblock, for the
        program's failure."""
        node = self.nodes[0]
        r = node.testmempoolaccept([tx_hex])[0]
        assert not r["allowed"] and (ANNEX_REFUSED in r["reject-reason"] or RELAY_ANNEX in r["reject-reason"]), (label, r)
        err = self.forced_block(tx_hex)
        assert err is not None and ANNEX_REFUSED in err, (label, err)
        self.log.info("REJECT %-52s mempool: %s | block: %s", label, r["reject-reason"], err)

    def run_test(self):
        node, parent = self.nodes[0], self.nodes[1]
        self.tool_bin = self.build_tool()
        self.sizes = {}
        self.generatetoaddress(parent, 5, parent.getnewaddress(), sync_fun=self.no_op)
        self.generate(node, 101)
        # The anchored chain's own policy asset (its genesis commits the parent's).
        self.policy = node.dumpassetlabels()["bitcoin"]
        status = node.getanchorstatus()
        self.log.info("anchored: %s", status)
        node.sendtoaddress(address=node.getnewaddress(), amount=1000000, fee_asset_label=self.policy)
        self.generate(node, 1)
        X, Y, Z = [node.issueasset(assetamount=1000000, tokenamount=0, blind=False, fee_asset=self.policy)["asset"]
                   for _ in range(3)]
        self.generate(node, 1)
        rates = node.getfeeexchangerates()
        rates.update({Y: 100000000, Z: 100000000})
        node.setfeeexchangerates(rates)
        internal = lambda d: bytes.fromhex(d)[::-1].hex()
        maker_spk, taker_spk, change_spk = self.wallet_spk(), self.wallet_spk(), self.wallet_spk()
        refund = compute_xonly_pubkey(generate_privkey())[0]
        expiry = node.getblockcount() + 1000

        derived = {}
        for version in (2, 1):
            S = {"a": internal(X), "b": internal(Y), "num": NUM, "den": DEN, "minlot": 1_000_000, "maker": maker_spk,
                 "expiry": expiry, "refund": refund, "version": version}
            d = self.tool(["simderive"] + self.sim_flags(S))
            assert_equal(node.getaddressinfo(d["address"]["elementsregtest"])["scriptPubKey"], d["scriptpubkey"])
            derived[version] = d
            self.log.info("ADDRESS version %d: %s, root %s, template %s (%s)", version, d["address"]["elementsregtest"],
                          d["cmr"], d["template_hash"], d["template"])
            spk = bytes.fromhex(d["scriptpubkey"])
            s0 = self.coin(spk, LOCKED, X)
            pay = self.coin(self.wallet_spk(), 30_000_000_000_000, Y)
            fee = self.coin(self.wallet_spk(), 1_000_000, Z)

            F1 = 6_000_000_000_001
            p1 = ceil_price(F1, NUM, DEN)
            stack = self.tool(["simwitness"] + self.sim_flags(S) + ["-path", "fill"])["stack"]
            assert_equal(stack[2], d["cmr"])
            tx = self.mktx([s0, pay, fee], [
                self.out(p1, maker_spk, Y), self.out(LOCKED - F1, spk, X),            # credit, rest
                self.out(F1, taker_spk, X), self.out(pay[2] - p1, change_spk, Y),
                self.out(fee[2] - FEE, change_spk, Z),
                CTxOut(CTxOutValue(FEE), b"", CTxOutAsset(b"\x01" + bytes.fromhex(Z)[::-1]))])
            good = self.finish(tx, {0: stack})
            c = self.simcheck(S, good, 0, LOCKED, True)
            assert c["ok"] and c["filled"] == F1 and c["required"] == p1, c
            r = node.testmempoolaccept([good])[0]
            assert r["allowed"], r
            self.log.info("CONTROL version %d partial fill without an annex: accepted, %d vB", version, r["vsize"])
            if version == 2:
                for n in (3, 1_000):
                    self.refuse_both("version 2 partial fill, %d-byte annex" % n, self.annexed(good, 0, n))
                self.refuse_both("version 2 partial fill, 1000-byte zero annex", self.annexed(good, 0, 1_000, b"\x00"))
                self.mine("version 2 partial fill (the control), k=0", good)
                continue
            for n in (1_000, 10_000):
                r = node.testmempoolaccept([self.annexed(good, 0, n)])[0]
                assert r["allowed"] or RELAY_ANNEX in r["reject-reason"], (n, r)
                self.log.info("BEFORE version 1 partial fill, %d-byte annex: the mempool %s", n,
                              "accepts it, %d vB (not broadcast)" % r["vsize"] if r["allowed"] else "refuses it: " + r["reject-reason"])
            padded = self.annexed(good, 0, 1_000)
            assert self.forced_block(padded) is None, "a block refused version 1's fill with an annex"
            txid = node.decoderawtransaction(padded)["txid"]
            assert_equal(len(node.getrawtransaction(txid, True)["vin"][0]["txinwitness"]), 5)
            self.log.info("BEFORE version 1 partial fill with a 1,000-byte annex nobody signed: mined in a block, %s", txid)
            # The remainder, still under version 1, fills whole as before.
            rest = (txid, 1, LOCKED - F1)
            change = (txid, 3, pay[2] - p1)
            node.lockunspent(False, [{"txid": txid, "vout": 3}])
            fee2 = self.coin(self.wallet_spk(), 1_000_000, Z)
            p2 = ceil_price(rest[2], NUM, DEN)
            stack_w = self.tool(["simwitness"] + self.sim_flags(S) + ["-path", "whole"])["stack"]
            w = self.mktx([rest, change, fee2], [
                self.out(p2, maker_spk, Y), self.out(rest[2], taker_spk, X), self.out(change[2] - p2, change_spk, Y),
                self.out(fee2[2] - FEE, change_spk, Z),
                CTxOut(CTxOutValue(FEE), b"", CTxOutAsset(b"\x01" + bytes.fromhex(Z)[::-1]))])
            w = self.finish(w, {0: stack_w})
            c = self.simcheck(S, w, 0, rest[2], False)
            assert c["ok"], c
            self.mine("version 1 whole fill of the remainder, k=0", w)
        assert derived[1]["scriptpubkey"] != derived[2]["scriptpubkey"], "one address for both versions"
        assert_equal(derived[1]["terms"], derived[2]["terms"])
        for k, v in self.sizes.items():
            self.log.info("SIZE %-46s %5d vB  %5d WU", k, v["vsize"], v["weight"])


if __name__ == "__main__":
    SeqObSimplicityAnnexTest().main()
