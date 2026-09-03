package adapter

import (
	"crypto/rand"
	"fmt"
)

// NewSessionID returns a random RFC 4122 version 4 UUID.
//
// agent-orc assigns the session ID at launch rather than discovering it
// afterwards: a CLI that accepts one is then resumable even if it writes
// nothing machine-readable about its own session, which is what the review
// loop needs to hand feedback back to the original worker.
func NewSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating a session id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
