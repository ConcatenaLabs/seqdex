package main

// takerkey.go: the same-chain lift's taker key, kept in a file instead of on a command line.
//
// A lift signs with a key that holds the coins it pays (-taker-priv) and unblinds them with a
// blinding key (-taker-blinding). Passed as flags, both show in the process list of every user
// on the machine, and a program that runs lifts (seqob-crosser) passes them on again. A key
// file holds both, mode 600, and `lift -taker-key-file` and `seqob-crosser -taker-key-file`
// read it: only its path is ever on a command line.
//
//   seqob-cli taker-key -file /path/taker.key [-net testnet]
//
// makes the file when it does not exist (two fresh keys) and prints the address to fund, as
// JSON: {"address": "<the unblinded address>", "confidential_address": "<its blinded form>"}.
// Both forms pay the same script. The keys themselves are never printed.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/vulpemventures/go-elements/payment"
)

// readTakerKeyFile reads a taker key file: two lines, "priv <64 hex>" and "blinding <64 hex>".
func readTakerKeyFile(path string) (priv, blinding string, err error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", "", err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return "", "", fmt.Errorf("%s is readable by others (mode %o): it holds a key; chmod 600 it", path, st.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		switch f[0] {
		case "priv":
			priv = f[1]
		case "blinding":
			blinding = f[1]
		}
	}
	for _, k := range []struct{ name, v string }{{"priv", priv}, {"blinding", blinding}} {
		if d, e := hex.DecodeString(k.v); e != nil || len(d) != 32 {
			return "", "", fmt.Errorf("%s: no 32-byte hex %q line", path, k.name)
		}
	}
	return priv, blinding, nil
}

// writeTakerKeyFile makes a new key file with two fresh keys; it never replaces one.
func writeTakerKeyFile(path string) error {
	var p, b [32]byte
	for _, k := range [][]byte{p[:], b[:]} {
		if _, err := rand.Read(k); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := fmt.Fprintf(f, "priv %s\nblinding %s\n", hex.EncodeToString(p[:]), hex.EncodeToString(b[:]))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// takerAddresses gives the unblinded and the blinded address of a taker key pair.
func takerAddresses(netName, priv, blinding string) (plain, conf string, err error) {
	pk, _ := btcec.PrivKeyFromBytes(mustHex32(priv, "priv"))
	bk, _ := btcec.PrivKeyFromBytes(mustHex32(blinding, "blinding"))
	p := payment.FromPublicKey(pk.PubKey(), selectNet(netName), bk.PubKey())
	if plain, err = p.WitnessPubKeyHash(); err != nil {
		return "", "", err
	}
	conf, err = p.ConfidentialWitnessPubKeyHash()
	return plain, conf, err
}

func cmdTakerKey(args []string) {
	fs := newFlagSet("taker-key")
	file := fs.String("file", "", "the taker key file: made (mode 600) when it does not exist")
	netName := fs.String("net", "testnet", "network: testnet|mainnet")
	_ = fs.Parse(args)
	if *file == "" {
		fatal("taker-key needs -file")
	}
	made := false
	if _, err := os.Stat(*file); errors.Is(err, os.ErrNotExist) {
		if err := writeTakerKeyFile(*file); err != nil {
			fatal("make %s: %v", *file, err)
		}
		made = true
	}
	priv, blinding, err := readTakerKeyFile(*file)
	if err != nil {
		fatal("%v", err)
	}
	plain, conf, err := takerAddresses(*netName, priv, blinding)
	if err != nil {
		fatal("address: %v", err)
	}
	out, _ := json.Marshal(map[string]interface{}{"address": plain, "confidential_address": conf, "made": made})
	fmt.Println(string(out))
}
