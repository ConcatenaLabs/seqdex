package domain

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	path "github.com/aejkcs50/seqdex/wallet/pkg/wallet/derivation-path"
	singlesig "github.com/aejkcs50/seqdex/wallet/pkg/wallet/single-sig"
)

// AccountInfo holds basic info about an account.
type AccountInfo struct {
	Namespace      string
	Label          string
	Xpub           string
	DerivationPath string
}

// GetMasterBlindingKey derives the account's SLIP-77 master blinding key
// from the wallet's mnemonic. It returns an error, never a panic, when the
// wallet is locked (no mnemonic in the store) or the account's derivation
// path cannot be read: a request answered between a lock and an unlock must
// not bring the daemon down.
func (i *AccountInfo) GetMasterBlindingKey() (string, error) {
	if MnemonicStore == nil {
		return "", ErrWalletLocked
	}
	mnemonic := MnemonicStore.Get()
	if len(mnemonic) <= 0 {
		return "", ErrWalletLocked
	}
	rootPath, err := path.ParseDerivationPath(i.DerivationPath)
	if err != nil {
		return "", err
	}
	if len(rootPath) <= 0 {
		return "", fmt.Errorf("account %s has an empty derivation path", i.Namespace)
	}
	rootPath = rootPath[:len(rootPath)-1]
	ww, err := singlesig.NewWalletFromMnemonic(singlesig.NewWalletFromMnemonicArgs{
		RootPath: rootPath.String(),
		Mnemonic: mnemonic,
	})
	if err != nil {
		return "", err
	}
	return ww.MasterBlindingKey()
}

// Account defines the entity data struture for a derived account of the
// daemon's HD wallet
type Account struct {
	AccountInfo
	Index                  uint32
	BirthdayBlock          uint32
	NextExternalIndex      uint
	NextInternalIndex      uint
	DerivationPathByScript map[string]string
	Unconf                 bool
}

func (a *Account) incrementExternalIndex() (next uint) {
	// restart from 0 if index has reached the its max value
	next = 0
	if a.NextExternalIndex != hdkeychain.HardenedKeyStart-1 {
		next = a.NextExternalIndex + 1
	}
	a.NextExternalIndex = next
	return
}

func (a *Account) incrementInternalIndex() (next uint) {
	next = 0
	if a.NextInternalIndex != hdkeychain.HardenedKeyStart-1 {
		next = a.NextInternalIndex + 1
	}
	a.NextInternalIndex = next
	return
}

func (a *Account) addDerivationPath(outputScript, derivationPath string) {
	if _, ok := a.DerivationPathByScript[outputScript]; !ok {
		a.DerivationPathByScript[outputScript] = derivationPath
	}
}
