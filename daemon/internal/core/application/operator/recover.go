package operator

import (
	"runtime/debug"

	log "github.com/sirupsen/logrus"
)

// recoverHandler, deferred at the top of a goroutine, logs a panic with its
// stack and ends that goroutine alone. The daemon serves trades and markets;
// one wallet notification it cannot classify must not take it down.
func recoverHandler(what string) {
	if p := recover(); p != nil {
		log.Errorf("operator: %s panicked, skipped: %v\n%s", what, p, debug.Stack())
	}
}
