# SeqDEX wallet daemon (Ocean fork)

A thin fork of the [Ocean](https://github.com/vulpemventures/ocean) wallet
daemon, adapted to Sequentia. It is the wallet the SeqDEX daemons settle
through: `tdexd` talks to it over the unchanged `ocean.v1` gRPC contract
(`api-spec/protobuf/ocean/v1`), and the SeqOB maker reuses its swap-completion
service. It is an operator/market-maker wallet, not an end-user wallet
(end-user wallets are built on
[SWK](https://github.com/ConcatenaLabs/SWK)).

Testnet software. The daemon manages its operator's own funds; it never holds
a counterparty's.

## What it does

- HD wallet (BIP39 mnemonic, SLIP-77 blinding) with named accounts, address
  derivation, coin selection, UTXO tracking.
- Builds, blinds, signs, and broadcasts PSETv2 transactions, including the
  cooperative atomic-swap completion (`CompleteSwap`) both DEX subsystems use.
  Blinding is per-call: Sequentia is transparent by default and confidential
  transactions are opt-in.
- Serves the `ocean.v1` gRPC interface on `:18000` (wallet, account,
  transaction, notification services). Client CLI: `cmd/ocean`.

## Sequentia changes relative to upstream Ocean

The fork is deliberately thin; the blinding/PSET/coin-selection core is generic
Elements code, untouched. What changed:

- **`pkg/seqnet`**: the Sequentia `network.Network` values. Addresses are
  deliberately Bitcoin-identical (bech32 `bc`/`tb`/`bcrt`, Bitcoin base58
  bytes); only the confidential (blech32) HRP is Sequentia-specific: `sqb`
  mainnet, `tsqb` testnet. Network names: `sequentia`, `sequentia-testnet`
  (default), `sequentia-regtest`.
- **Runtime policy asset**: the policy asset (tSEQ) id is genesis-derived, so
  it is not a compile-time constant. `OCEAN_NATIVE_ASSET` is required and read
  from the node (`getsidechaininfo.pegged_asset`).
- **The `elements` blockchain scanner** reads block and header structure via
  JSON-RPC verbosity instead of raw-header deserialization, because
  go-elements cannot parse Sequentia's anchored PoS headers. Individual
  transactions still decode with go-elements (Sequentia is wire-compatible for
  transactions). The scanner polls the node tip (~2s) and needs
  `-blockfilterindex=1` on the node; a block deeper than six below the tip
  that has no filter is read in full. Each watch resumes at the first block it
  has not scanned, and each account's scanned height is saved every minute in
  `<datadir>/<scanner dir>/checkpoints.json`, so after a restart the watches set
  as the wallet opens resume there (twelve blocks back) instead of scanning
  again from the account's birth; an address or coin watched later still
  starts from the account's birth. A vendored neutrino-elements scanner
  variant exists as well (`internal/infrastructure/blockchain-scanner/`).
- **Open fee market**: non-swap transfers pay the network fee in the
  transacted asset (node-fed exchange rates) rather than assuming a native fee
  asset; swap-side any-asset fees live in the daemon module's `CompleteSwap`.
- Robustness fixes: non-200 / JSON-RPC-error node responses return errors
  instead of panicking; `importaddress` with `rescan=false` in `GetUtxos`.

## Configuration

Env-only, prefix `OCEAN_` (full list in `internal/config/config.go`):

| Env | Meaning | Default |
|---|---|---|
| `OCEAN_NETWORK` | `sequentia` / `sequentia-testnet` / `sequentia-regtest` | `sequentia-testnet` |
| `OCEAN_NATIVE_ASSET` | policy asset hex (**required**; from `getsidechaininfo.pegged_asset`) | - |
| `OCEAN_NODE_RPC_ADDR` | node JSON-RPC as a full URL `http://user:pass@host:port` | - |
| `OCEAN_BLOCKCHAIN_SCANNER_TYPE` | `elements` (node-RPC scanner) | `elements` |
| `OCEAN_ESPLORA_URL` | optional Esplora endpoint (unset = node-RPC-only) | - |
| `OCEAN_PORT` | gRPC listen port | `18000` |
| `OCEAN_DATADIR`, `OCEAN_DB_TYPE` | state directory, `badger`/`inmemory`/`postgres` | `~/.seqdex-wallet`, `badger` |
| `OCEAN_NO_TLS` | disable TLS (dev) | `false` |

## Build and run

```sh
go build -o ../bin/seqdex-wallet ./cmd/oceand
go build -o ../bin/ocean         ./cmd/ocean     # CLI
```

Run against a local Sequentia regtest (how to start one, and why it must be an
Elements-mode chain: [../docs/DEV.md](../docs/DEV.md)):

```sh
OCEAN_NETWORK=sequentia-regtest \
OCEAN_NATIVE_ASSET=<pegged_asset hex> \
OCEAN_NODE_RPC_ADDR=http://s:s@127.0.0.1:19996 \
OCEAN_BLOCKCHAIN_SCANNER_TYPE=elements \
OCEAN_DB_TYPE=badger OCEAN_NO_TLS=true \
OCEAN_DATADIR=./oceand-data \
./bin/seqdex-wallet
```

Then, with the CLI: `ocean config init --no-tls`, `ocean wallet create` /
`unlock`, `ocean account create`, `ocean account derive`, `ocean account
balance`. The verified smoke flow is: create, unlock, create an account, derive
an address, fund it from the node, watch the balance sync.

## Tests

```sh
go test ./pkg/...        # passes (wallet math, derivation, single-sig)
```

The upstream suites under `internal/core/application` and
`internal/core/domain` predate the Sequentia network adaptation and currently
fail with "unknown network"; treat them as upstream-inherited, not as coverage
of the Sequentia paths. End-to-end coverage of this wallet comes from the
daemon module's swap flows and the regtest loops in
[../docs/DEV.md](../docs/DEV.md).

## Upstream

Upstream Ocean docs: https://github.com/vulpemventures/ocean. The `ocean.v1`
protos here are kept byte-identical to upstream so the daemon-wallet seam
stays drop-in; MIT notices are retained in [../NOTICE](../NOTICE).
