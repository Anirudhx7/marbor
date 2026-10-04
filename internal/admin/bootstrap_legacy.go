package admin

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Anirudhx7/marbor/internal/bootstrapcred"
	"github.com/Anirudhx7/marbor/internal/store"
)

// migrateLegacyAdmin carries the legacy single-admin credential into the users
// table. A bcrypt hash is carried over as is. A hash the bcrypt-only login
// cannot verify (the old iterated SHA-256 form) is replaced by the documented
// default password with a forced change and the skip counter at its cap, keeping
// the legacy username, so the account is recoverable instead of permanently
// locked out; that account is recorded with the legacy_recovery origin so the
// non-loopback refusal applies to it at once.
//
// It reports whether the legacy table was handled, in which case the caller
// must not create a fresh admin.
func (s *Server) migrateLegacyAdmin() bool {
	has, err := s.st.HasAdminCredentials()
	if err != nil {
		s.failBootstrap(fmt.Errorf("could not check for legacy administrator credentials: %w", err))
		return true
	}
	if !has {
		return false
	}
	uname, hash, salt, err := s.st.GetLegacyAdminCreds()
	if errors.Is(err, store.ErrNoAdminCreds) {
		return false
	}
	if err != nil {
		// Do not create a fresh admin beside credentials that exist but cannot
		// be read: that would hide the operator's real account.
		s.failBootstrap(fmt.Errorf("could not read the legacy administrator credentials: %w", err))
		return true
	}
	uname = strings.TrimSpace(uname)
	if uname == "" {
		uname = "admin"
	}
	if err := s.legacyNameFree(uname); err != nil {
		s.failBootstrap(err)
		return true
	}
	if !isBcryptHash(hash) {
		s.recoverLegacyAdmin(uname)
		return true
	}
	if _, err := s.st.CreateUser(store.User{
		Username:     uname,
		Role:         "admin",
		Status:       "active",
		PasswordHash: hash,
		Salt:         salt,
		CreatedAt:    time.Now(),
	}); err != nil {
		// Do not fall through to the fresh-install branch: that would create a
		// second admin and hide the real account from the operator.
		s.failBootstrap(fmt.Errorf("could not migrate legacy admin %q to the users table: %w", uname, err))
		return true
	}
	log.Printf("admin: migrated legacy credentials to users table (username: %q)", uname)
	s.bootstrapExisting()
	return true
}

// legacyNameFree fails with an actionable error when a user row (active,
// suspended or soft-deleted) already holds the legacy username. Creating the
// account would hit the unique constraint on every start and fail closed forever
// behind an opaque store error, so say what to do instead.
func (s *Server) legacyNameFree(uname string) error {
	_, err := s.st.GetUserByUsername(uname)
	switch {
	case err == nil:
		return fmt.Errorf("the legacy administrator %q cannot be migrated: a user with that name already exists (possibly suspended or deleted). Rename or permanently remove that user, then restart", uname)
	case errors.Is(err, store.ErrUserNotFound):
		return nil
	}
	return fmt.Errorf("could not check whether the legacy administrator name %q is free: %w", uname, err)
}

// recoverLegacyAdmin recreates a legacy admin whose stored hash can never
// authenticate on the public default password, with a forced change, no skip,
// and a legacy_recovery marker. If any step cannot be stored the account is
// removed again rather than left skippable or unmarked.
func (s *Server) recoverLegacyAdmin(uname string) {
	newHash, err := hashPassword(defaultAdminPassword)
	if err != nil {
		s.failBootstrap(fmt.Errorf("could not hash recovery password for legacy admin %q: %w", uname, err))
		return
	}
	if err := s.createMarkedAdmin(uname, newHash, bootstrapcred.OriginLegacyRecovery, false); err != nil {
		s.failBootstrap(fmt.Errorf("could not migrate legacy admin %q to the users table: %w", uname, err))
		return
	}
	logLegacyAdminRecoveryWarning(uname)
	s.refreshBootstrapPending()
}
