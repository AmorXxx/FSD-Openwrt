package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const (
	forwardGuardChain        = "fsd_forward_guard"
	forwardReadySet          = "fsd_forward_ready"
	forwardReadyLeaseSeconds = 5
	clearForwardReady        = "flush set inet fw4 " + forwardReadySet + "\n"
)

func (app *application) maintainForwardGuard() {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, os.Interrupt)
	defer signal.Stop(stop)
	defer func() {
		if err := nft(clearForwardReady); err != nil {
			log.Printf("FSD forwarding guard close: %v", err)
		}
	}()

	// Replace the lease in one transaction. If FSD exits unexpectedly, it expires.
	renew := clearForwardReady + fmt.Sprintf(
		"add element inet fw4 %s { %q timeout %ds }\n", forwardReadySet, app.lanDevice, forwardReadyLeaseSeconds)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	ready := false
	var lastError string
	for {
		if err := nft(renew); err != nil {
			if lastError != err.Error() {
				log.Printf("FSD forwarding guard renew: %v", err)
				lastError = err.Error()
			}
			ready = false
		} else if !ready {
			log.Print("FSD 就绪，客户端出网已放行")
			ready = true
			lastError = ""
		}
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
	}
}
