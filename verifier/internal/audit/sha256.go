package audit

import "crypto/sha256"

// sha256Sum is a thin wrapper for the verifier's standalone use of
// sha256. Splitting it out keeps verify.go free of the crypto import
// (which collides with the helper-name namespace some readers expect
// for the chain functions) without dropping standardness — the
// algorithm is exactly the stdlib's sha256.
func sha256Sum(b []byte) [sha256.Size]byte {
	return sha256.Sum256(b)
}
