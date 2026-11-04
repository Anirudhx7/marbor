package admin

import "golang.org/x/crypto/bcrypt"

// Tests exercise hashing behavior, not its work factor; DefaultCost made
// every NewServer (which hashes the default admin password) cost seconds
// under the race detector.
func init() { bcryptCost = bcrypt.MinCost }
