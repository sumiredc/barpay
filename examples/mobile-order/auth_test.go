package main

import (
	"os"
	"strings"
	"testing"
)

// productionHashIterations is hashIterations as the app starts with it, recorded before
// TestMain lowers it.
var productionHashIterations = hashIterations

// TestMain lowers the iteration count so that the tests, which hash staff passwords, dummy
// hashes and logins many times, do not spend their time in PBKDF2. A hash records its own count,
// so checking a password follows the same path with fewer iterations.
func TestMain(m *testing.M) {
	hashIterations = 1
	os.Exit(m.Run())
}

// Passwords are hashed with 600,000 iterations outside the tests, and the hash records that
// count, so lowering it for the tests does not reach the app or `hash-password`.
func TestPasswordsAreHashedWith600000IterationsOutsideTests(t *testing.T) {
	if productionHashIterations != 600_000 {
		t.Fatalf("hashIterations starts as %d, want 600000", productionHashIterations)
	}
	lowered := hashIterations
	hashIterations = productionHashIterations
	defer func() { hashIterations = lowered }()

	hash := hashPassword("correct horse")
	if parts := strings.Split(hash, "$"); len(parts) != 4 || parts[1] != "600000" {
		t.Fatalf("hashPassword gave %q, want the count 600000 in it", hash)
	}
	if !checkPassword(hash, "correct horse") {
		t.Error("checkPassword refused the password the hash was made from")
	}
}
