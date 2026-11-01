package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A shelled leg runs seqob-cli with the node URLs and the taker's keys in its
// environment: the process the crosser starts carries none of them on its
// command line, where every user on the machine can read them.
func TestShelledLegCarriesNoCredentialOnItsCommandLine(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "seen")
	cli := filepath.Join(dir, "seqob-cli")
	script := "#!/bin/sh\n" +
		"{ echo \"ARGV $*\"; echo \"SEQ=$SEQOB_SEQ_RPC\"; echo \"BTC=$SEQOB_BTC_RPC\"; echo \"PRIV=$SEQOB_TAKER_PRIV\"; } > " + out + "\n" +
		"echo 'SWAP SETTLED'\n"
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	e := liftExecutor()
	e.Caps.SeqobCli, e.Caps.LegTimeout = cli, time.Minute
	e.Caps.BtcRPC, e.Caps.BtcWallet, e.Caps.BtcChain = "http://u:secretbtc@h:2", "b", "testnet4"
	e.Caps.SeqRPC = "http://u:secretseq@h:1"
	n := norm(mkOffer(FamCross, SideAsk, tBase, tQuote, 100, 4800), tBase, tQuote)
	args, _, _, err := e.legCommand(n, 50)
	if err != nil {
		t.Fatal(err)
	}
	settled, err := e.runCLI(args)
	if err != nil || !settled {
		t.Fatalf("runCLI: settled %v, err %v", settled, err)
	}
	seen, _ := os.ReadFile(out)
	s := string(seen)
	argv := strings.SplitN(s, "\n", 2)[0]
	for _, secret := range []string{"secretbtc", "secretseq", "-taker-priv", "-seq-rpc", "-btc-rpc"} {
		if strings.Contains(argv, secret) {
			t.Fatalf("seqob-cli command line carries %q: %s", secret, argv)
		}
	}
	for _, want := range []string{"SEQ=http://u:secretseq@h:1", "BTC=http://u:secretbtc@h:2", "PRIV=11"} {
		if !strings.Contains(s, want) {
			t.Fatalf("seqob-cli environment lacks %q:\n%s", want, s)
		}
	}
}
