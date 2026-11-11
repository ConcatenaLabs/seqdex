#!/usr/bin/env bash
# A real bitcoind (the BTC leg, and the anchor's parent) and a Sequentia
# node anchored to it with -validateanchor=1 and -par=1, on regtest. Wallets:
# bitcoind w (miner), taker, maker; sequentiad w (miner), taker, maker. The
# maker issues the swap asset; the node prices it so it pays its own fee.
#
#   NAMEDLOCK_DIR=/fresh/dir NAMEDLOCK_PORT=39443 bash start-regtest-bitcoind.sh up|stop
#
# Every PID is written to $NAMEDLOCK_DIR/pids; `stop` stops those only.
set -euo pipefail
DIR="${NAMEDLOCK_DIR:?a fresh directory}"
SEQD="${SEQUENTIAD:-$HOME/Sequentia/src/sequentiad}"
SEQC="$(dirname "$SEQD")/sequentia-cli"
BTCD="${BITCOIND:-/usr/local/bin/bitcoind}"
BTCC="$(dirname "$BTCD")/bitcoin-cli"
BTC_RPC=${NAMEDLOCK_PORT:-39443}; BTC_P2P=$((BTC_RPC+1)); SEQ_RPC=$((BTC_RPC+58)); SEQ_P2P=$((BTC_RPC+59))
EXTRA_SEQ_ARGS=${NAMEDLOCK_SEQ_ARGS:-}
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

mkdir -p "$DIR/btc" "$DIR/seq"
: > "$DIR/pids"
"$BTCD" -datadir="$DIR/btc" -regtest -port=$BTC_P2P -rpcport=$BTC_RPC -txindex=1 -fallbackfee=0.0001 \
  -server -printtoconsole=0 -listen=0 >/dev/null 2>&1 & echo $! >> "$DIR/pids"
for _ in $(seq 1 480); do btc getblockcount >/dev/null 2>&1 && break; sleep 0.5; done
GENESIS="$(btc getblockhash 0)"
for w in w taker maker; do btc -named createwallet wallet_name=$w >/dev/null; done
btc -rpcwallet=w generatetoaddress 110 "$(btc -rpcwallet=w getnewaddress)" >/dev/null
for w in taker maker; do btc -rpcwallet=w sendtoaddress "$(btc -rpcwallet=$w getnewaddress)" 20 >/dev/null; done
btc -rpcwallet=w generatetoaddress 1 "$(btc -rpcwallet=w getnewaddress)" >/dev/null
IFS=: read -r BU BP < "$DIR/btc/regtest/.cookie" || true

# shellcheck disable=SC2086
"$SEQD" -datadir="$DIR/seq" -chain=elementsregtest -port=$SEQ_P2P -rpcport=$SEQ_RPC -listen=0 \
  -initialfreecoins=2100000000000000 -anyonecanspendaremine=1 -blindedaddresses=0 \
  -con_default_blinded_addresses=0 -validatepegin=0 -con_any_asset_fees=1 -maxtxfee=100.0 \
  -txindex=1 -par=1 -fallbackfee=0.0001 -debug=mempool \
  -con_bitcoin_anchor=1 -validateanchor=1 -anchorpollinterval=1 \
  -mainchainrpchost=127.0.0.1 -mainchainrpcport=$BTC_RPC -mainchainrpcuser="$BU" -mainchainrpcpassword="$BP" \
  -parentgenesisblockhash="$GENESIS" -server -printtoconsole=0 $EXTRA_SEQ_ARGS >/dev/null 2>&1 & echo $! >> "$DIR/pids"
for _ in $(seq 1 480); do sq getblockcount >/dev/null 2>&1 && break; sleep 0.5; done
for w in w taker maker; do sq -named createwallet wallet_name=$w descriptors=false >/dev/null; done
SADDR="$(sq -rpcwallet=w getnewaddress)"
sq -rpcwallet=w generatetoaddress 101 "$SADDR" >/dev/null
GTX="$(sq scantxoutset start '["raw(51)"]' | python3 -c 'import json,sys;u=json.load(sys.stdin)["unspents"][0];print(u["txid"]+":"+str(u["vout"]))')"
RAW="$(sq createrawtransaction "[{\"txid\":\"${GTX%:*}\",\"vout\":${GTX#*:}}]" "[{\"$SADDR\":20999999.99},{\"fee\":0.01}]")"
sq sendrawtransaction "$RAW" >/dev/null
sq -rpcwallet=w generatetoaddress 1 "$SADDR" >/dev/null
for w in taker maker; do sq -rpcwallet=w -named sendtoaddress address="$(sq -rpcwallet=$w getnewaddress)" amount=1000 fee_asset_label=bitcoin >/dev/null; done
sq -rpcwallet=w generatetoaddress 1 "$SADDR" >/dev/null
ASSET="$(sq -rpcwallet=maker -named issueasset assetamount=100000 tokenamount=0 blind=false fee_asset=bitcoin | python3 -c 'import json,sys;print(json.load(sys.stdin)["asset"])')"
sq -rpcwallet=w generatetoaddress 1 "$SADDR" >/dev/null
RATES="$(sq getfeeexchangerates | python3 -c "import json,sys;r=json.load(sys.stdin);r['$ASSET']=100000000;print(json.dumps(r))")"
sq setfeeexchangerates "$RATES" >/dev/null
IFS=: read -r SU SP < "$DIR/seq/elementsregtest/.cookie" || true

cat > "$DIR/env" <<ENV
NAMEDLOCK_BTC_RPC=127.0.0.1:$BTC_RPC:$BU:$BP
NAMEDLOCK_SEQ_RPC=127.0.0.1:$SEQ_RPC:$SU:$SP
NAMEDLOCK_ASSET=$ASSET
ENV
echo up
