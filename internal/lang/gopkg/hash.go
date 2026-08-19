package gopkg

import (
	"crypto/sha256"
	"encoding/hex"
)

// Hash is the fingerprint function: sha256 over normalised source (spec §6.4).
func Hash(norm []byte) string {
	sum := sha256.Sum256(norm)
	return "sha256:" + hex.EncodeToString(sum[:])
}
