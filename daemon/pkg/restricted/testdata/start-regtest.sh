#!/usr/bin/env bash
# Bring up what a restricted swap runs against, on regtest: a bitcoind (the
# BTC leg, and the anchor's parent), a Sequentia node anchored to it with -par=1
# (so a block refused for a script names the failure), and openampd (the
# asset's policy server) on that node. Prints the variables the test reads.
#
#   SEQUENTIAD=... BITCOIND=... OPENAMPD=... TOKEN_COVENANTS=... \
#   RESTRICTED_E2E_DIR=/path/to/a/fresh/dir bash start-regtest.sh up|stop
#
# Every process's PID is written to $DIR/pids, and `stop` stops those only.
set -euo pipefail
DIR="${RESTRICTED_E2E_DIR:?a fresh directory for the run}"
SEQD="${SEQUENTIAD:?sequentiad}"
SEQC="$(dirname "$SEQD")/sequentia-cli"
BTCD="${BITCOIND:-bitcoind}"
BTCC="$(dirname "$(command -v "$BTCD")")/bitcoin-cli"
AMPD="${OPENAMPD:?openampd}"
TOOL="${TOKEN_COVENANTS:?token-covenants}"
BTC_RPC=28443; BTC_P2P=28444; SEQ_RPC=28501; SEQ_P2P=28502; AMP=28600
btc() { "$BTCC" -datadir="$DIR/btc" -regtest -rpcport=$BTC_RPC "$@"; }
sq() { "$SEQC" -datadir="$DIR/seq" -chain=elementsregtest -rpcport=$SEQ_RPC "$@"; }

case "${1:-up}" in
stop)
  if [ -f "$DIR/pids" ]; then
    while read -r pid; do kill "$pid" 2>/dev/null || true; done < "$DIR/pids"
  fi
  exit 0 ;;
up) ;;
*) echo "usage: $0 up|stop" >&2; exit 2 ;;
esac

mkdir -p "$DIR/btc" "$DIR/seq" "$DIR/amp"
: > "$DIR/pids"
"$BTCD" -datadir="$DIR/btc" -regtest -port=$BTC_P2P -rpcport=$BTC_RPC -txindex=1 -fallbackfee=0.0001 \
  -server -printtoconsole=0 -listen=0 >/dev/null 2>&1 & echo $! >> "$DIR/pids"
for _ in $(seq 1 120); do btc getblockcount >/dev/null 2>&1 && break; sleep 0.5; done
GENESIS="$(btc getblockhash 0)"
btc -named createwallet wallet_name=w >/dev/null
btc -rpcwallet=w generatetoaddress 101 "$(btc -rpcwallet=w getnewaddress)" >/dev/null
IFS=: read -r BU BP < "$DIR/btc/regtest/.cookie" || true

"$SEQD" -datadir="$DIR/seq" -chain=elementsregtest -port=$SEQ_P2P -rpcport=$SEQ_RPC -listen=0 \
  -initialfreecoins=2100000000000000 -anyonecanspendaremine=1 -blindedaddresses=0 \
  -con_default_blinded_addresses=0 -validatepegin=0 -con_any_asset_fees=1 -maxtxfee=100.0 \
  -txindex=1 -par=1 -fallbackfee=0.0001 \
  -con_bitcoin_anchor=1 -validateanchor=1 -anchorpollinterval=1 \
  -mainchainrpchost=127.0.0.1 -mainchainrpcport=$BTC_RPC -mainchainrpcuser="$BU" -mainchainrpcpassword="$BP" \
  -parentgenesisblockhash="$GENESIS" -server -printtoconsole=0 >/dev/null 2>&1 & echo $! >> "$DIR/pids"
for _ in $(seq 1 120); do sq getblockcount >/dev/null 2>&1 && break; sleep 0.5; done
sq -named createwallet wallet_name=w descriptors=false >/dev/null
SADDR="$(sq -rpcwallet=w getnewaddress)"
sq -rpcwallet=w generatetoaddress 101 "$SADDR" >/dev/null
# The genesis free coins sit at OP_TRUE: claim them into the wallet.
GTX="$(sq scantxoutset start '["raw(51)"]' | python3 -c 'import json,sys;u=json.load(sys.stdin)["unspents"][0];print(u["txid"]+":"+str(u["vout"]))')"
RAW="$(sq createrawtransaction "[{\"txid\":\"${GTX%:*}\",\"vout\":${GTX#*:}}]" "[{\"$SADDR\":20999999.99},{\"fee\":0.01}]")"
sq sendrawtransaction "$RAW" >/dev/null
sq -rpcwallet=w generatetoaddress 1 "$SADDR" >/dev/null
FEE_ASSET="$(sq -rpcwallet=w -named issueasset assetamount=1000000 tokenamount=0 blind=false fee_asset=bitcoin | python3 -c 'import json,sys;print(json.load(sys.stdin)["asset"])')"
sq -rpcwallet=w generatetoaddress 1 "$SADDR" >/dev/null
RATES="$(sq getfeeexchangerates | python3 -c "import json,sys;r=json.load(sys.stdin);r['$FEE_ASSET']=100000000;print(json.dumps(r))")"
sq setfeeexchangerates "$RATES" >/dev/null
IFS=: read -r SU SP < "$DIR/seq/elementsregtest/.cookie" || true

COLD="$("$TOOL" key --new --key-file "$DIR/amp/cold.key" | python3 -c 'import json,sys;print(json.load(sys.stdin)["key"])')"
"$AMPD" -listen 127.0.0.1:$AMP -datadir "$DIR/amp/data" -rpc http://127.0.0.1:$SEQ_RPC -rpcauth "$SU:$SP" \
  -rpcwallet w -issuertoken e2e-issuer-token -demoissuer -feeasset "$FEE_ASSET" -feesats 5000 \
  -follow 200ms -tokentool "$TOOL" -capcoldkey "$COLD" > "$DIR/amp/openampd.log" 2>&1 & echo $! >> "$DIR/pids"
for _ in $(seq 1 120); do curl -sf http://127.0.0.1:$AMP/v1/assets >/dev/null && break; sleep 0.5; done

cat > "$DIR/env" <<ENV
RESTRICTED_BTC_RPC=127.0.0.1:$BTC_RPC:$BU:$BP
RESTRICTED_SEQ_RPC=127.0.0.1:$SEQ_RPC:$SU:$SP
RESTRICTED_POLICY=http://127.0.0.1:$AMP
RESTRICTED_ISSUER_TOKEN=e2e-issuer-token
RESTRICTED_FEE_ASSET=$FEE_ASSET
ENV
echo up
