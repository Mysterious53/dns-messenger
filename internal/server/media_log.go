package server

import (
	"log"
	"sync/atomic"
)

// mediaDebugLogs gates verbose media-cache log output. Server.Run flips it
// based on the --debug flag at startup. Atomic so other goroutines reading
// the value while logging don't need a mutex.
var mediaDebugLogs atomic.Bool

// SetMediaDebugLogs enables or disables the media debug log channel.
func SetMediaDebugLogs(enabled bool) {
	mediaDebugLogs.Store(enabled)
}

// logfMedia prints a media-feature log line only when debug logging is on.
// Errors that operators should always see go through plain log.Printf
// directly; logfMedia is reserved for the chatty per-store / per-cache-hit
// chatter.
func logfMedia(format string, args ...interface{}) {
	if !mediaDebugLogs.Load() {
		return
	}
	log.Printf("[media-debug] "+format, args...)
}
