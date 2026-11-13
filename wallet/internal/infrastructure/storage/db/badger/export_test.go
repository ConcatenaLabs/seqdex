package dbbadger

import "github.com/aejkcs50/seqdex/wallet/internal/core/domain"

// SetWalletLoadedHook makes repo call f each time it has read the wallet's file, before it
// keeps what it read: a test holds a read there while it writes or resets.
func SetWalletLoadedHook(repo domain.WalletRepository, f func()) {
	repo.(*walletRepository).loaded = f
}
