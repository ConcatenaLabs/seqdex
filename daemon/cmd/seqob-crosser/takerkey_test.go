package main

import (
	"strings"
	"testing"
)

// With -taker-key-file the crosser can run same-chain lifts, and the lift it shells out names
// the file by its path: neither key is on seqob-cli's command line.
func TestSameChainLiftTakesTheKeyFileByPath(t *testing.T) {
	n := norm(mkOffer(FamSameChain, SideAsk, tBase, tQuote, 100, 333), tBase, tQuote)
	e := covExecutor()
	if ok, why := e.Caps.Executable(n); ok || !strings.Contains(why, "-taker-key-file") {
		t.Fatalf("without the seams: executable %v (%s), want refused naming -taker-key-file", ok, why)
	}
	e.Caps.Esplora, e.Caps.TakerKeyFile = "http://esplora", "/etc/x/taker.key"
	if ok, why := e.Caps.Executable(n); !ok {
		t.Fatalf("with -esplora and -taker-key-file: refused (%s)", why)
	}
	args, _, _, err := e.legCommand(n, 10)
	if err != nil {
		t.Fatal(err)
	}
	_, f := argsMap(t, args)
	if f["-taker-key-file"] != "/etc/x/taker.key" || f["-esplora"] != "http://esplora" {
		t.Fatalf("lift args %v: want -esplora and -taker-key-file", args)
	}
	for _, a := range args {
		if a == "-taker-priv" || a == "-taker-blinding" {
			t.Fatalf("lift args %v carry %s", args, a)
		}
	}
	// the two keys given as flags reach seqob-cli in its environment, never its command line
	le := liftExecutor()
	args, _, _, _ = le.legCommand(n, 10)
	for _, a := range args {
		if a == "-taker-priv" || a == "-taker-blinding" {
			t.Fatalf("lift args %v carry %s", args, a)
		}
	}
	env := le.Caps.childEnv()
	if !hasEnv(env, "SEQOB_TAKER_PRIV=11") || !hasEnv(env, "SEQOB_TAKER_BLINDING=22") {
		t.Fatalf("child env lacks the taker keys")
	}
}

func hasEnv(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}
