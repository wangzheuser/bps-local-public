package gateway

import "crypto/rand"

// readRandom fills the buffer with cryptographic randomness, terminating the
// process only when the OS entropy source fails irrecoverably.
func readRandom(b []byte) (int, error) {
	return rand.Read(b)
}
