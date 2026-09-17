# SeqDEX

The Sequentia DEX: a non-custodial order-book and atomic-swap system, in Go. Forked from TDEX
(`tdex-daemon`, `tdex-protobuf`) and Ocean.

Everything here is testnet software. Node and consensus conventions live in the
[`Sequentia`](https://github.com/ConcatenaLabs/Sequentia) repo.

## Branches

The remote default is `main`, and it is the trunk: branch from it, open pull requests against
it. `phase3-pure-ln` was the development branch for most of 2026 and diverged from `main` (each
carried commits the other lacked, including the same fix landed twice under one title). The two
were merged on 2026-08-23 and `main` was fast-forwarded to the result, so they now point at the
same history; `phase3-pure-ln` stays only so old checkouts and the box still resolve it. Do not
branch from it.

The docs (`README.md`, `docs/ARCHITECTURE.md`, `docs/DEV.md`) were brought up to date on
2026-08-22; `docs/ARCHITECTURE.md` is kept as a 2026-07-08 snapshot with a header listing what
has changed since. Read them, then verify against code.

## Modules and layout

Seven Go modules; the root one only covers `proto/gen/go`. The two that matter are `daemon/` and
`wallet/`.

The Go import path is `github.com/aejkcs50/seqdex/...` while the GitHub remote is
`ConcatenaLabs/seqdex`. That is a historical username, not a mistake — do not
"fix" the module path.

There is **no committed `go.work`**; it is kept local by design.

- `daemon/internal/seqob/` — the order book: `api`, `client`, `matcher`, `offer`, `offerstore`,
  `covfill`, `reorg`, `session`, `settler`, `validator`, `watcher`, `bridge`.
- `daemon/pkg/xchain/` — the shared cross-chain HTLC engine.
- `daemon/pkg/covenant/` — the covenant CLOB leaf builder.
- `wallet/` — the Ocean fork, the Sequentia wallet daemon.

## The naming trap: `xchain` is LIVE

The retired RFQ rail and the live order-book cross rail were **both** called "xchain". The RFQ
rail was deleted (its protos, its `xchainmaker` application package, and the `seqdex-xchaind` /
`seqdex-xchain-taker` / `seqdex-xchain-reverse-taker` binaries). What survived, deliberately:

- **`daemon/pkg/xchain/` is the live cross taker and is imported by about 40 files** —
  `seqob-maker`, `seqob-cli`, `seqob-settler`, the `internal/seqob/client/xdriver*.go` drivers,
  `seqob-crosser` on `phase3-pure-ln`, and even `tdexd`'s fee-rate handler. Deleting it breaks
  nearly everything.
- `daemon/cmd/seqdex-xchain-swapdemo` is kept for the same reason: despite its name it exercises
  `pkg/xchain` and holds no reference to the retired service.
- The `x`-prefixed files under `daemon/internal/seqob/client/` (`xcourier*.go`, `xdriver*.go`,
  `xminslice.go`) are all live.
- "RFQ" survives **only in comments** describing the retired rail. There is no code named `rfq`.

**Draw the dead-code boundary by reachability from a live entry point, never by name match.**
`daemon/pkg/xchain/maker.go` carries an explicit warning header saying exactly this, because its
previous header claimed it served the retired service and that was already false when written.

One consequence, since removed: `daemon/api-spec/protobuf/seqdex/gen-seqdex.sh` kept looping
over an `xchain` proto that no longer existed, and its file-existence guard hid that.

## What runs on the server

| Binary | Role |
|---|---|
| `seqobd` | The relay. Stores signed offers, serves the book over REST and WS, couriers opaque end-to-end-encrypted swap messages. **Holds no wallet, no keys and no funds.** The chain watcher runs as a goroutine inside it when given `-node-rpc`. |
| `seqob-maker` | The maker. Modes: `samechain`, `cross`, `lightning`, `pureln`, `subasset`, `subasset-sell`. |
| `seqob-crosser` | Crossing agent; polls every relay and takes both sides. |
| `seqob-settler` | Always-online non-custodial settler for the passive covenant CLOB (`plan`, `run`). |
| `seqob-bridge` | Cross-rail settlement bridge; the only seqob component holding its own inventory and LN node. |
| `tdexd` | The forked LP daemon. Deployed under the name `seqdexd` — there is no `cmd/seqdexd`. |
| `oceand` | The wallet daemon, from the `wallet/` module. |

The box runs **several `seqobd` relay instances**, one per rail; the `seqob-crosser` `-relays`
default documents the fleet (9955 cross, 9965 pure-LN and submarine, 9966 sub-asset buy, 9971
sub-asset sell, plus the confidential relay on 9975). Publicly they are the `/seqob`,
`/seqob-pln` and `/seqob-conf` mounts.

The relay holds its book in memory (only the trade log is durable, via `-trade-log`). Restarting
a relay empties its book until every maker re-posts, which the Go makers do on reconnect; an
offer posted by something that is not running at the time (a wallet's covenant order) comes
back only when that client re-posts it.

Everything else in `cmd/` is a one-shot CLI or a regtest helper.

**There are no systemd units, deploy scripts or supervisor scripts in this repo** — not now and
not in its history. Deployment is pull-only: the server pulls from GitHub and builds there. Never
edit source on the server, and never copy source onto it.

## Keys

Maker identity keys are supplied at launch as a hex flag (`-maker-priv`) or generated in-process.
Nothing reads a key from a path inside this repo. They live outside it, and they must never be
committed.

**Each maker needs its own identity key.** A shared key cross-wired live swaps three times on the
sub-asset rail, each time stranding a funded taker HTLC until its CLTV refund.

Per-swap recovery material (session keys and legs) is written under the maker's `-xstate-dir` and
the crosser's `-state-dir`. Those directories are the difference between a recoverable
interruption and a stranded swap.

## Build and test

There is **no CI**. Nothing runs unless you run it.

```sh
cd daemon && go build ./...
cd ../wallet && go build ./...

cd daemon
go build -o ../bin/seqobd       ./cmd/seqobd
go build -o ../bin/seqob-maker  ./cmd/seqob-maker
go build -o ../bin/seqob-cli    ./cmd/seqob-cli
go build -o ../bin/tdexd        ./cmd/tdexd

cd daemon  && go test ./internal/seqob/... ./pkg/xchain/...
cd ../wallet && go test ./pkg/...
```

`daemon/Makefile` also offers `test` (`-race`), `cov`, `vet`, `lint` (golangci-lint), `proto`,
`mock` and `integrationtest`.

Known non-signals, per `main`'s `docs/DEV.md`: `wallet/internal/core/...` tests fail with "unknown
network", and `daemon/test/e2e` (`make integrationtest`) has never been adapted to Sequentia.

`make run` is stale: it exports `TDEX_*`, but the config reader's env prefix is `SEQDEX`. The only
remaining `TDEX_` consumer is the operator CLI's own datadir variable. The upstream
docker-compose file is stale in the same way.

Build binaries with `-o` into `bin/`. Running `go build ./cmd/<name>` from inside `daemon/` drops
stray binaries in `daemon/`, which has happened often enough that `.gitignore` has entries for
them by name.

Regtest needs Elements mode (`-con_elementsmode=1`); a plain `sequentiad -chain=regtest` runs in
Bitcoin serialization mode and the parsers cannot decode it. The node binaries are `sequentiad`
and `sequentia-cli`; `pkg/xchain/testdata/start-regtest.sh` falls back to the legacy
`elementsd` names only for a clone built before the rename.

## Correctness rules with a history behind them

- **Never die mid-swap.** `seqob-maker` drains in-flight lifts on SIGTERM before exiting.
- **Makers exit on purpose and must run under a supervisor.** An offer expiring silently emptied
  the book, so a maker now exits shortly before its own offers expire and expects to be restarted.
  The supervisor is not in this repo.
- **Counterparty-supplied values are clamped, always.** The driver marks the counterparty-controlled
  fields explicitly and never trusts a courier-supplied amount.
- **The native asset is not 1:1 for fees.** Sizing a fee as though it were under-pays. Fees are
  sized from the published rate, for every asset including the native one.
- **Offer canonical bytes are a cross-language contract.** The signed canonical form excludes the
  covenant outpoint. The JavaScript side in `sequentia-web-wallet` re-implements this and the
  covenant byte order; a change on either side must land on both.
- Byte order at the covenant boundary is display-vs-internal, and the wallet mirrors this repo's
  `reverseHex`/`displayHex` conventions. Golden vectors pin the two implementations together.

## Working in this repo

- **Repository is public.** Never commit keys, seeds, wallet files, RPC credentials, `.env` files
  or tokens.
- **Commit author:**
  `GracedEternalKingCabbageMan <151803062+GracedEternalKingCabbageMan@users.noreply.github.com>`
- **Always open a pull request, then merge it yourself immediately.** The PR exists so the change
  and its reasoning are recorded, not because anyone is waiting to review it. There is no review
  process. If you are ever told to leave one specific PR open, that applies to that PR only and
  never becomes the default.

<!-- BEGIN SHARED AGENT CONVENTIONS: identical in every Sequentia repo. Change it in all of them together. -->
## Working with git and GitHub here

These rules are the same in every Sequentia repository. They are repeated in each
one because this file is the only thing an agent is guaranteed to read, whatever
machine it is working from.

**Nothing pushed to GitHub credits Claude, Anthropic, or any AI tool.** No
`Co-Authored-By: Claude` trailer, no `Claude-Session:` trailer or `claude.ai`
link, no "Generated with Claude Code" in a commit message or a pull request body,
no `claude/*` branch names or session ids, and no mention in source, comments,
docs or issue text. Agent tooling offers several of these by default; compose the
message without them rather than stripping them afterwards.

**Author every commit as the person the session is working for.** Several people
commit in these repositories and an agent always runs on behalf of one of them,
so derive the author from the authenticated GitHub account rather than from a
list of names that goes stale the moment somebody new arrives:

    git -c user.name="$(gh api user --jq '.name // .login')" \
        -c user.email="$(gh api user --jq '"\(.id)+\(.login)@users.noreply.github.com"')" \
        commit ...

That address is the GitHub `noreply` form, which is what links a commit to its
account and keeps private addresses out of a public history. When `gh` is not
authenticated as the person the work belongs to, ask them instead of guessing.

**Never infer the author from `git log`.** The clones carry no `user.name` or
`user.email`, so `git commit` stops with "Author identity unknown" and the
nearest answer to hand is the author of the last commit — which is whoever
pushed last and says nothing about who is working now. Attributing a commit to
someone who did not write it puts their name on code they never reviewed, and
taking it back costs a history rewrite and a force-push over commits other
machines have already pulled.

**Every change lands through a pull request that you merge yourself, at once.**
There is no reviewer on this project; the pull request exists so the reasoning is
recorded beside the diff. Branch, push, open it, merge it, delete the branch, all
in one sitting. Pushing straight to the default branch is the rule most often
broken here, and it is the one that costs the record. A pull request stays open
only when the repository owner asks for that specific one, and that never carries
over to the next.

**Name branches `area/short-description`**: `fix/`, `doc/`, `feature/`, `test/`,
`build/`, or the component being changed. Never a tool name, a session id, or
`worktree-*`.

**Write the subject as `area: what changed`**, one line, 72 characters at the
outside and 50 where you can manage it. Put the reasoning in the body, and
explain why rather than what.

**These repositories are public and world-readable.** Never commit private keys,
seeds, `wallet.dat`, RPC credentials, `.env` files or API tokens. Read the diff
before every commit. Secrets belong on the server and in offline backups.

**A file belongs to the repository whose code it describes.** Decide which repo
owns it before writing it; if it landed in the wrong one, move it rather than
deleting it.

**Documentation is part of the change, not a follow-up.** A change that makes a
README, a doc page, a runbook or a code comment wrong is not finished until that
text is right again, in the same pull request as the code. Before you open the
pull request, search the repository for whatever you renamed, moved or removed —
the old binary name, the old path, the old flag, the old command — and fix every
hit. If the change falsifies another repository's documentation, that repository
gets its own pull request in the same sitting. A stale instruction costs a new
user more than a missing one: they trust it, run it, it fails, and the failure
reads as broken software rather than as an out-of-date sentence.

**Write documentation to be timeless.** Assume the reader is new, arrived today,
and wants to know what the software is and how to use it right now. They do not
care what changed, what it used to be called, or which version added what. So
write in the present tense about current behaviour, and leave the history out:
no changelogs, no "new in", no "recently", no "coming soon", no status or
progress sections, no roadmaps, no dated notes. Quote a version number only where
the reader cannot act without it, and prefer pointing at the file that carries it
over copying the digits. Timeless does not mean thin — what the product is, who
it is for, and how to install, configure and use it all still belong there, in
full. Documentation written this way survives a release without an edit, which is
what keeps it true; the history already has homes in the git log, the tags and
the release notes.

**Push the same day you commit.** The testnet server pulls only from GitHub, so a
branch left on one laptop is invisible to every other machine and to the box.
<!-- END SHARED AGENT CONVENTIONS -->
