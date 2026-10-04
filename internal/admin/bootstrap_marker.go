package admin

import (
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/Anirudhx7/marbor/internal/bootstrapcred"
	"github.com/Anirudhx7/marbor/internal/store"
)

// loadMarker reads the pending-initial-password marker. A missing or blank
// value is (nil, nil).
func (s *Server) loadMarker() (*bootstrapcred.Marker, error) {
	raw, err := s.st.GetSetting(bootstrapcred.SettingKey)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	return bootstrapcred.DecodeMarker(raw)
}

func (s *Server) saveMarker(m bootstrapcred.Marker) error {
	raw, err := m.Encode()
	if err != nil {
		return err
	}
	return s.st.SetSetting(bootstrapcred.SettingKey, raw)
}

// clearMarker blanks the marker (the settings store has no delete).
func (s *Server) clearMarker() error {
	return s.st.SetSetting(bootstrapcred.SettingKey, "")
}

// activeDefaultAdmins returns the active admins whose password still verifies
// against the public default. A store error is returned, never swallowed: the
// callers decide how to fail.
func (s *Server) activeDefaultAdmins() ([]store.User, error) {
	users, err := s.st.ListUsers()
	if err != nil {
		return nil, fmt.Errorf("could not list users: %w", err)
	}
	var out []store.User
	for _, u := range users {
		if u.Role == "admin" && u.Status == "active" && verifyPassword(u.PasswordHash, bootstrapcred.DefaultPassword) {
			out = append(out, u)
		}
	}
	return out, nil
}

// classifyExisting is the safety net for installs that have no marker: an
// active admin whose hash verifies against the public default is recorded. An
// account on the default password whose skip counter is already at the cap is
// the state the legacy recovery leaves behind and is enforced immediately,
// whether or not a forced change is still pending; any other default-password
// account is an upgraded install and gets one release of warn-only grace.
func (s *Server) classifyExisting() error {
	defaults, err := s.activeDefaultAdmins()
	if err != nil {
		return err
	}
	if len(defaults) == 0 {
		return nil
	}
	u := defaults[0]
	origin, reason := bootstrapcred.OriginUpgraded, "an existing default-password administrator without the recovery state"
	if u.SkipPasswordCount >= maxSkipPasswordChanges {
		origin, reason = bootstrapcred.OriginLegacyRecovery, "default password with the skip limit already reached"
	}
	m := bootstrapcred.Marker{Username: u.Username, CreatedAt: time.Now().UTC(), Origin: origin}
	s.boot.mu.Lock()
	defer s.boot.mu.Unlock()
	if err := s.saveMarker(m); err != nil {
		return fmt.Errorf("could not record the default-password marker: %w", err)
	}
	log.Printf("admin: administrator %q is still on the default password; classified as %q (%s)", u.Username, origin, reason)
	return nil
}

// reconcileMarkerFromFile covers a crash between creating the admin row and
// writing the marker: the generated password file is still there, the marker is
// not. When the file's password verifies against an active admin that must
// change its password, the marker is recreated (origin fresh) and the skip cap
// restored, instead of treating the file as stale and deleting the only copy of
// that password. It reports whether a marker was recreated.
func (s *Server) reconcileMarkerFromFile() (bool, error) {
	if s.boot.dir == "" {
		return false, nil
	}
	path := bootstrapcred.FilePath(s.boot.dir)
	exists, err := bootstrapcred.Exists(path)
	if err != nil || !exists {
		return false, err
	}
	password, err := bootstrapcred.ReadSafe(path)
	if errors.Is(err, bootstrapcred.ErrEmptySecretFile) {
		return false, nil // nothing to preserve; the empty file is removed as stale
	}
	users, listErr := s.st.ListUsers()
	if listErr != nil {
		return false, fmt.Errorf("could not list users: %w", listErr)
	}
	if err != nil {
		// An unsafe file cannot be trusted as a credential. When an admin is
		// still on the public default password the file is not what protects
		// that account, so it is just stale and gets removed; otherwise the
		// flow fails closed.
		for _, u := range users {
			if u.Role == "admin" && u.Status == "active" && verifyPassword(u.PasswordHash, defaultAdminPassword) {
				log.Printf("WARNING: ignoring %s: %v", path, err)
				return false, nil
			}
		}
		return false, err
	}
	for _, u := range users {
		if u.Role != "admin" || u.Status != "active" || !u.MustChangePassword || !verifyPassword(u.PasswordHash, password) {
			continue
		}
		return true, s.restoreMarkerFor(u, false)
	}
	return false, nil
}

// restoreMarkerFor re-establishes the fresh marker and the skip cap for an admin
// found on an initial password whose marker was lost. supplied records
// that the credential must never be regenerated.
func (s *Server) restoreMarkerFor(u store.User, supplied bool) error {
	if u.SkipPasswordCount < maxSkipPasswordChanges {
		u.SkipPasswordCount = maxSkipPasswordChanges
		if err := s.st.UpdateUser(u); err != nil {
			return fmt.Errorf("could not disable skipping the forced password change: %w", err)
		}
	}
	s.boot.mu.Lock()
	defer s.boot.mu.Unlock()
	m := bootstrapcred.Marker{Username: u.Username, CreatedAt: u.CreatedAt.UTC(), Origin: bootstrapcred.OriginFresh, Supplied: supplied}
	if err := s.saveMarker(m); err != nil {
		return fmt.Errorf("could not record the pending initial password: %w", err)
	}
	log.Printf("admin: administrator %q is still on an initial password; the pending-password marker was recreated", u.Username)
	return nil
}

// reevaluateMarker applies the transfer and clear rules after the marked user
// was deleted, suspended, demoted or had its password changed outside the
// normal hook. The marker stays while the marked user is still an active admin
// on an initial password; otherwise it moves to another default-password admin
// (origin and created_at kept) and is cleared only when none remains.
func (s *Server) reevaluateMarker() error {
	s.boot.mu.Lock()
	defer s.boot.mu.Unlock()
	return s.reevaluateMarkerLocked()
}

func (s *Server) reevaluateMarkerLocked() error {
	m, err := s.loadMarker()
	if err != nil {
		return fmt.Errorf("could not read the first-boot marker: %w", err)
	}
	if m == nil {
		return nil
	}
	users, err := s.st.ListUsers()
	if err != nil {
		return fmt.Errorf("could not list users: %w", err)
	}
	for _, u := range users {
		if u.Username != m.Username || u.Role != "admin" || u.Status != "active" {
			continue
		}
		if u.MustChangePassword || verifyPassword(u.PasswordHash, bootstrapcred.DefaultPassword) {
			return nil // still on an initial password
		}
		// The marked user has changed its password and no longer verifies
		// against the default: the marker is stale.
		return s.settleAfterMarkedChangeLocked(*m)
	}
	return s.moveOrClearMarkerLocked(*m, "no active administrator remains on an initial password")
}

// moveOrClearMarkerLocked moves the marker to another default-password admin
// when one exists (origin and created_at kept), otherwise clears it together
// with the password file. The caller holds s.boot.mu.
func (s *Server) moveOrClearMarkerLocked(m bootstrapcred.Marker, why string) error {
	next, err := s.activeDefaultAdmins()
	if err != nil {
		return err
	}
	if len(next) > 0 {
		moved := m
		moved.Username = next[0].Username
		if err := s.saveMarker(moved); err != nil {
			return fmt.Errorf("could not move the default-password marker to %q: %w", moved.Username, err)
		}
		log.Printf("admin: default-password marker moved from %q to %q", m.Username, moved.Username)
		return nil
	}
	return s.clearMarkerAndFileLocked(why)
}

// clearMarkerAndFileLocked clears the marker and removes the password file. The
// caller holds s.boot.mu. The file is removed even when the marker cannot be
// cleared: by this point the password it holds is no longer the way in. A marker
// that could not be cleared is logged and returned, and is re-evaluated (and
// cleared) at the next start.
func (s *Server) clearMarkerAndFileLocked(why string) error {
	markerErr := s.clearMarker()
	if markerErr != nil {
		log.Printf("admin: could not clear the initial-password marker (%s); it is re-checked at the next start: %v", why, markerErr)
	} else {
		log.Printf("admin: initial admin password is no longer pending (%s)", why)
	}
	if s.boot.dir != "" {
		s.removePasswordFile(bootstrapcred.FilePath(s.boot.dir))
	}
	return markerErr
}

// settleAfterMarkedChangeLocked handles the marked user's password change: the
// password file always goes, and the marker is cleared unless another active
// admin is still on the public default, in which case it moves there with its
// origin and created_at kept. The caller holds s.boot.mu.
func (s *Server) settleAfterMarkedChangeLocked(m bootstrapcred.Marker) error {
	next, err := s.activeDefaultAdmins()
	if err != nil {
		return err
	}
	if len(next) > 0 {
		moved := m
		moved.Username = next[0].Username
		if err := s.saveMarker(moved); err != nil {
			return fmt.Errorf("could not move the default-password marker to %q: %w", moved.Username, err)
		}
		log.Printf("admin: initial password changed for %q; default-password marker moved to %q", m.Username, moved.Username)
		if s.boot.dir != "" {
			s.removePasswordFile(bootstrapcred.FilePath(s.boot.dir))
		}
		return nil
	}
	return s.clearMarkerAndFileLocked("the password was changed")
}

// onPasswordChanged is the single hook for every successful password change
// or reset. When the marked user's password changed, the marker is cleared and
// the password file is removed. It then refreshes the cached pending flag. All
// of it runs under s.boot.mu.
func (s *Server) onPasswordChanged(username string) {
	s.boot.mu.Lock()
	defer s.boot.mu.Unlock()
	m, err := s.loadMarker()
	switch {
	case err != nil:
		log.Printf("admin: could not read the first-boot marker after a password change: %v", err)
	case m != nil && m.Username == username:
		if err := s.settleAfterMarkedChangeLocked(*m); err != nil {
			log.Printf("admin: could not settle the initial-password state after a password change: %v", err)
		}
	}
	s.refreshBootstrapPendingLocked()
}

// onUserAccessChanged is the hook for deleting, suspending, demoting or
// reactivating a user.
func (s *Server) onUserAccessChanged() {
	s.boot.mu.Lock()
	defer s.boot.mu.Unlock()
	if err := s.reevaluateMarkerLocked(); err != nil {
		log.Printf("admin: could not re-evaluate the initial-password marker after a user change: %v", err)
	}
	s.refreshBootstrapPendingLocked()
}

// refreshBootstrapPending recomputes whether an initial/default password is
// active and stores it, all under s.boot.mu so a stale result cannot overwrite
// a newer one.
func (s *Server) refreshBootstrapPending() {
	s.boot.mu.Lock()
	defer s.boot.mu.Unlock()
	s.refreshBootstrapPendingLocked()
}

// refreshBootstrapPendingLocked computes the flag: the marker is present, or
// any active admin still verifies against the public default. When the store
// cannot be read the flag is set (fail closed) and the error is logged. The
// caller holds s.boot.mu.
func (s *Server) refreshBootstrapPendingLocked() {
	m, err := s.loadMarker()
	if err != nil {
		log.Printf("admin: could not read the first-boot marker, treating an initial password as pending: %v", err)
		s.boot.pendingState.Store(&pendingSnapshot{pending: true})
		return
	}
	defaults, err := s.activeDefaultAdmins()
	if err != nil {
		log.Printf("admin: could not check for default-password administrators, treating an initial password as pending: %v", err)
		s.boot.pendingState.Store(&pendingSnapshot{pending: true})
		return
	}
	src := bootstrapcred.PasswordSourceDefault
	if m != nil {
		src = m.PasswordSource()
		// A fresh marker that moved to another account on the public default
		// no longer points at a generated or supplied secret.
		for _, u := range defaults {
			if u.Username == m.Username {
				src = bootstrapcred.PasswordSourceDefault
			}
		}
	}
	s.boot.pendingState.Store(&pendingSnapshot{pending: m != nil || len(defaults) > 0, source: src})
}

// BootstrapPasswordPending reports whether an initial/default admin password
// is still active. It is a cached flag and never carries a secret.
func (s *Server) BootstrapPasswordPending() bool {
	return s.boot.isPending()
}

// BootstrapPasswordSource reports where the pending password comes from:
// "file" (generated into the data directory), "supplied" (configured by the
// operator) or "default" (the public default). It is empty when nothing is
// pending or the source is unknown. It is a cached value and never carries a
// secret.
func (s *Server) BootstrapPasswordSource() bootstrapcred.PasswordSource {
	snap := s.boot.pendingState.Load()
	if snap == nil || !snap.pending {
		return ""
	}
	return snap.source
}

// warnBootstrapPending logs, on every boot while it is true, that an initial
// or default admin password is still active. It never logs a password.
func (s *Server) warnBootstrapPending() {
	if !s.boot.isPending() {
		return
	}
	defaults, err := s.activeDefaultAdmins()
	if err != nil {
		log.Printf("admin: could not list default-password administrators for the startup warning: %v", err)
	}
	for _, u := range defaults {
		logDefaultAdminCredentialWarning(u.Username, u.MustChangePassword)
	}
	if m, err := s.loadMarker(); err == nil && m != nil && m.Origin == bootstrapcred.OriginFresh {
		log.Printf("WARNING: the initial admin password for %q has not been changed yet; the dashboard accepts it over plaintext HTTP", m.Username)
	}
}
