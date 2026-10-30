package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// taker-key's file: made once with mode 600, never replaced, read back the same; a file others
// can read, or one without both keys, is refused; the address pays the same script blinded or not.
func TestTakerKeyFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "taker.key")
	if err := writeTakerKeyFile(p); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o, want 600", st.Mode().Perm())
	}
	priv, blind, err := readTakerKeyFile(p)
	if err != nil || len(priv) != 64 || len(blind) != 64 || priv == blind {
		t.Fatalf("read back: %q %q %v", priv, blind, err)
	}
	if err := writeTakerKeyFile(p); err == nil {
		t.Fatal("a second write replaced the key file")
	}
	if p2, b2, _ := readTakerKeyFile(p); p2 != priv || b2 != blind {
		t.Fatal("the key file changed")
	}
	plain, conf, err := takerAddresses("testnet", priv, blind)
	if err != nil || !strings.HasPrefix(plain, "tb1q") || conf == plain || conf == "" {
		t.Fatalf("addresses %q %q %v: want an unblinded tb1q and a distinct blinded form", plain, conf, err)
	}
	_ = os.Chmod(p, 0o644)
	if _, _, err := readTakerKeyFile(p); err == nil || !strings.Contains(err.Error(), "readable by others") {
		t.Fatalf("a 644 key file: %v", err)
	}
	_ = os.Chmod(p, 0o600)
	_ = os.WriteFile(p, []byte("priv "+priv+"\n"), 0o600)
	if _, _, err := readTakerKeyFile(p); err == nil || !strings.Contains(err.Error(), "blinding") {
		t.Fatalf("a file without the blinding key: %v", err)
	}
}
