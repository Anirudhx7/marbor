package admin

import (
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Anirudhx7/marbor/internal/bootstrapcred"
	"github.com/Anirudhx7/marbor/internal/store"
)

// BootstrapOptions tells NewServerWithBootstrap where the first-boot admin
// password file lives and how to read the environment. The zero value is
// valid: no data directory (so a fresh install cannot store a generated
// password and fails closed unless the operator supplies one) and os.Getenv.
type BootstrapOptions struct {
	// DataDir is the directory that holds the database; the generated
	// password file is created inside it.
	DataDir string
	// Getenv reads environment variables; nil means os.Getenv.
	Getenv func(string) string
	// GraceActive decides whether the one-release warn-only grace applies to a
	// build version. Nil means bootstrapcred.GraceActive, under which a source
	// or development build has no grace. It exists so tests can pin a release
	// without weakening that rule.
	GraceActive func(version string) bool
}

// pendingSnapshot is one consistent view of the pending-password state.
type pendingSnapshot struct {
	pending bool
	// source is a bootstrapcred.PasswordSource, or "" when unknown.
	source bootstrapcred.PasswordSource
}

// isPending reports the cached pending flag.
func (b *bootstrapState) isPending() bool {
	snap := b.pendingState.Load()
	return snap != nil && snap.pending
}

// bootstrapState is the first-boot credential state of one Server.
type bootstrapState struct {
	dir         string
	getenv      func(string) string
	graceActive func(string) bool
	// mu serializes read-modify-write of the stored marker and the cached
	// pending flag.
	mu sync.Mutex
	// err is set once during construction when the first-boot credential
	// could not be set up safely; the admin listener must not start.
	err error
	// source is where an operator-supplied initial secret came from at this
	// start (SourceNone when none was supplied).
	source bootstrapcred.SecretSource
	// pendingState caches "an initial/default password is active" and where it
	// comes from as one immutable snapshot swapped atomically, so health checks
	// never run bcrypt and never see a flag paired with another refresh's source.
	// Nil until the first refresh.
	pendingState atomic.Pointer[pendingSnapshot]
	// bindOverride is the environment-forced admin bind, when one is in
	// effect. It is never persisted.
	bindOverride string
	// envSecretSeen records that MARBOR_ADMIN_PASSWORD was set at start, after
	// it has been unset in the process environment.
	envSecretSeen bool
}

func newBootstrapState(opts BootstrapOptions) bootstrapState {
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	grace := opts.GraceActive
	if grace == nil {
		grace = bootstrapcred.GraceActive
	}
	return bootstrapState{dir: opts.DataDir, getenv: getenv, graceActive: grace}
}

// scrubSuppliedSecret unsets the plain-text initial password in this process's
// environment once boot has read it, so child processes the server starts do not
// inherit it. That is all it does: it does not clear /proc/<pid>/environ (the
// kernel keeps the initial environment block), `docker inspect`, or the
// container or unit definition, so the operator should supply the password with
// a secrets file (MARBOR_ADMIN_PASSWORD_FILE) or remove it from the definition
// after the first boot. It only touches the real process environment
// (processEnv); a custom Getenv belongs to the caller.
func (s *Server) scrubSuppliedSecret(processEnv bool) {
	s.boot.envSecretSeen = s.boot.getenv(bootstrapcred.EnvPassword) != ""
	if !processEnv || !s.boot.envSecretSeen {
		return
	}
	if err := os.Unsetenv(bootstrapcred.EnvPassword); err != nil {
		log.Printf("WARNING: could not remove %s from the process environment: %v", bootstrapcred.EnvPassword, err)
	}
}

// failBootstrap records that the first-boot credential flow failed closed. It
// is only called while the server is being constructed.
func (s *Server) failBootstrap(err error) {
	s.boot.err = err
	log.Printf("ERROR: admin dashboard will not start: the first-boot administrator credential could not be set up safely: %v", err)
}

// ensureAdminUser sets up the administrator on first run and, on every later
// start, works out whether an initial/default password is still active.
//
// With no administrator at all it either migrates the legacy credential or
// creates an admin with a generated password (or one the operator supplied).
// The secret is stored first (private file or environment), then the marker, then
// the user row in a single write that already carries the forced change and the
// skip cap, so a failed or interrupted boot is retryable and never leaves an
// admin that is skippable or unrecorded. Every store error on the way fails the
// first-boot flow closed instead of being ignored.
func (s *Server) ensureAdminUser() {
	if _, isNop := s.st.(store.NopStore); isNop {
		return
	}
	count, err := s.st.CountAdminUsers()
	if err != nil {
		s.failBootstrap(fmt.Errorf("could not count administrators: %w", err))
		return
	}
	if count > 0 {
		s.bootstrapExisting()
		return
	}
	if s.migrateLegacyAdmin() {
		return
	}
	s.bootstrapFresh()
}

// maxAdminUsernameTries bounds the search for a free admin username.
const maxAdminUsernameTries = 50

// freeAdminUsername returns "admin" when no user (active, suspended or
// soft-deleted) holds that name, otherwise the first free "admin-N". A
// soft-deleted or demoted user keeps its row, so reusing its name would fail on
// the unique constraint forever.
func (s *Server) freeAdminUsername() (string, error) {
	for i := 1; i <= maxAdminUsernameTries; i++ {
		name := "admin"
		if i > 1 {
			name = fmt.Sprintf("admin-%d", i)
		}
		_, err := s.st.GetUserByUsername(name)
		if errors.Is(err, store.ErrUserNotFound) {
			return name, nil
		}
		if err != nil {
			return "", fmt.Errorf("could not check whether the username %q is free: %w", name, err)
		}
	}
	return "", fmt.Errorf("no free administrator username among admin..admin-%d; remove or rename an existing user", maxAdminUsernameTries)
}

// bootstrapFresh creates the first administrator on a new install.
func (s *Server) bootstrapFresh() {
	password, source, err := bootstrapcred.ResolveSecret(s.boot.getenv)
	s.boot.source = source
	if err != nil {
		s.failBootstrap(err)
		return
	}
	username, err := s.freeAdminUsername()
	if err != nil {
		s.failBootstrap(err)
		return
	}
	path := bootstrapcred.FilePath(s.boot.dir)
	if source == bootstrapcred.SourceNone {
		if s.boot.dir == "" {
			s.failBootstrap(fmt.Errorf("no data directory is configured for the initial admin password file"))
			return
		}
		if password, err = s.generatedPassword(path); err != nil {
			s.failBootstrap(err)
			return
		}
	} else if err := s.removeLeftoverGeneratedFile(path); err != nil {
		s.failBootstrap(err)
		return
	}
	if err := s.createBootstrapAdmin(username, password, source != bootstrapcred.SourceNone); err != nil {
		s.failBootstrap(err)
		return
	}
	if source == bootstrapcred.SourceNone {
		log.Printf("admin: created administrator %q with a generated password; it is stored in %s (owner-only) and is removed after the first password change", username, path)
	} else {
		log.Printf("admin: created administrator %q with the password supplied through the environment; a password change is required at first login", username)
	}
	s.refreshBootstrapPending()
}

// removeLeftoverGeneratedFile deletes a generated password file left by an
// earlier start when the operator now supplies the secret: the supplied secret
// wins, and a leftover file would otherwise sit beside it as a second, stale
// credential.
func (s *Server) removeLeftoverGeneratedFile(path string) error {
	if s.boot.dir == "" {
		return nil
	}
	exists, err := bootstrapcred.Exists(path)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	log.Printf("WARNING: removing the generated initial admin password file %s: the operator supplied the initial password instead", path)
	return bootstrapcred.Remove(path)
}

// generatedPassword returns the password for a new install: the existing
// private file's contents when a previous boot already wrote one, otherwise a
// fresh random password written to path. An empty file (a crash between
// creating it and writing the secret) holds nothing to preserve and is
// replaced.
func (s *Server) generatedPassword(path string) (string, error) {
	exists, err := bootstrapcred.Exists(path)
	if err != nil {
		return "", err
	}
	if exists {
		pw, err := bootstrapcred.ReadSafe(path)
		if err == nil {
			return pw, nil
		}
		if !errors.Is(err, bootstrapcred.ErrEmptySecretFile) {
			return "", err
		}
		log.Printf("WARNING: removing the empty initial admin password file %s left by an interrupted earlier start; a new password is generated", path)
		if err := bootstrapcred.Remove(path); err != nil {
			return "", err
		}
	}
	password, err := bootstrapcred.Generate()
	if err != nil {
		return "", err
	}
	if err := bootstrapcred.WriteNew(path, password); err != nil {
		return "", err
	}
	return password, nil
}

// createBootstrapAdmin creates the first administrator on the given password;
// supplied says the operator chose it rather than the server generating it.
func (s *Server) createBootstrapAdmin(username, password string, supplied bool) error {
	hash, err := hashPassword(password)
	if err != nil {
		return fmt.Errorf("could not hash the initial admin password: %w", err)
	}
	return s.createMarkedAdmin(username, hash, bootstrapcred.OriginFresh, supplied)
}

// createMarkedAdmin creates an admin that must change its password, cannot skip
// the change, and is recorded in the marker with the given origin.
//
// The marker is written FIRST and the user row second, in one write that already
// carries the forced change and the skip cap. A crash between the two leaves a
// marker and no user, which the next start (no administrator yet) simply
// overwrites; the reverse order could leave a skippable, unrecorded admin that
// the gate would treat as an ordinary account. If the user write fails the
// marker is cleared again.
func (s *Server) createMarkedAdmin(username, hash string, origin bootstrapcred.Origin, supplied bool) error {
	m := bootstrapcred.Marker{Username: username, CreatedAt: time.Now().UTC(), Origin: origin, Supplied: supplied}
	s.boot.mu.Lock()
	defer s.boot.mu.Unlock()
	if err := s.saveMarker(m); err != nil {
		return fmt.Errorf("could not record the pending initial password: %w", err)
	}
	user := store.User{
		Username:           username,
		Role:               "admin",
		Status:             "active",
		PasswordHash:       hash,
		MustChangePassword: true,
		SkipPasswordCount:  maxSkipPasswordChanges,
		CreatedAt:          time.Now(),
	}
	if _, err := s.st.CreateUser(user); err != nil {
		if clearErr := s.clearMarker(); clearErr != nil {
			log.Printf("admin: could not clear the pending-password marker after the admin user could not be stored: %v", clearErr)
		}
		return fmt.Errorf("could not persist the admin user: %w", err)
	}
	return nil
}

// bootstrapExisting runs at every start of an already-set-up install:
// classify a pre-existing default-password admin, re-check the marker, and
// deal with the password file. Any store error fails the flow closed.
func (s *Server) bootstrapExisting() {
	m, err := s.loadMarker()
	if err != nil {
		s.failBootstrap(fmt.Errorf("could not read the first-boot marker: %w", err))
		return
	}
	if m == nil {
		recovered, err := s.reconcileMarkerFromFile()
		if err != nil {
			s.failBootstrap(err)
			return
		}
		if !recovered {
			if err := s.classifyExisting(); err != nil {
				s.failBootstrap(err)
				return
			}
		}
	} else if err := s.reevaluateMarker(); err != nil {
		s.failBootstrap(err)
		return
	}
	if err := s.settlePasswordFile(); err != nil {
		s.failBootstrap(err)
		return
	}
	s.refreshBootstrapPending()
	s.warnBootstrapPending()
}

// settlePasswordFile verifies the generated password file while it is still
// needed and removes a stale one once the marker is gone. It never deletes the
// file when the marker could not be read. A pending GENERATED credential whose
// file is missing or empty (a crash artefact or a deleted file) gets a new
// generated password so the install is never left without a way in. An
// operator-SUPPLIED credential is never regenerated or overwritten, whether or
// not the supplying variable is still set: a missing file or variable then means
// the operator's own source is gone, and that is theirs to restore.
func (s *Server) settlePasswordFile() error {
	if s.boot.dir == "" {
		return nil
	}
	path := bootstrapcred.FilePath(s.boot.dir)
	m, err := s.loadMarker()
	if err != nil {
		return fmt.Errorf("could not read the first-boot marker, so the password file %s was left in place: %w", path, err)
	}
	exists, err := bootstrapcred.Exists(path)
	if err != nil {
		return err
	}
	fresh := m != nil && m.Origin == bootstrapcred.OriginFresh && !m.Supplied
	switch {
	case !exists && fresh:
		return s.regeneratePendingPassword(path, *m)
	case !exists:
		return nil
	case m == nil:
		log.Printf("WARNING: removing stale initial admin password file %s: no initial password is pending", path)
		s.removePasswordFile(path)
		return nil
	case m.Supplied:
		log.Printf("WARNING: removing stale generated password file %s: the pending initial password was supplied by the operator", path)
		s.removePasswordFile(path)
		return nil
	case !fresh:
		// An upgraded or legacy-recovery marker never uses a generated file, so a
		// leftover one is stale and must not fail the flow closed through its
		// permissions.
		log.Printf("WARNING: removing stale generated password file %s: the pending initial password did not come from it", path)
		s.removePasswordFile(path)
		return nil
	}
	_, err = bootstrapcred.ReadSafe(path)
	if errors.Is(err, bootstrapcred.ErrEmptySecretFile) {
		log.Printf("WARNING: the initial admin password file %s is empty; replacing it with a new generated password", path)
		s.removePasswordFile(path)
		return s.regeneratePendingPassword(path, *m)
	}
	return err
}

// regeneratePendingPassword gives the marked administrator a new generated
// password and writes it to path. It does nothing unless that user is still an
// active admin that must change its password, and not when the environment
// still supplies a secret that works. The user row is updated before the file is
// written, so an interruption between the two leaves no file and the next start
// repeats this.
func (s *Server) regeneratePendingPassword(path string, m bootstrapcred.Marker) error {
	if m.Supplied {
		return nil // an operator-supplied credential is never replaced
	}
	u, err := s.st.GetUserByUsername(m.Username)
	if errors.Is(err, store.ErrUserNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("could not look up the administrator %q: %w", m.Username, err)
	}
	// GetUserByUsername also returns soft-deleted rows, so check for deletion
	// explicitly instead of relying on deletion having suspended the user.
	if u.DeletedAt != nil || u.Role != "admin" || u.Status != "active" || !u.MustChangePassword {
		return nil
	}
	if pw, source, envErr := bootstrapcred.ResolveSecret(s.boot.getenv); envErr == nil && source != bootstrapcred.SourceNone && verifyPassword(u.PasswordHash, pw) {
		return nil
	}
	password, err := bootstrapcred.Generate()
	if err != nil {
		return err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return fmt.Errorf("could not hash the regenerated admin password: %w", err)
	}
	u.PasswordHash = hash
	if err := s.st.UpdateUser(u); err != nil {
		return fmt.Errorf("could not store the regenerated admin password: %w", err)
	}
	if err := bootstrapcred.WriteNew(path, password); err != nil {
		return err
	}
	log.Printf("WARNING: the initial admin password file for %q was missing or empty; a new generated password was written to %s", m.Username, path)
	return nil
}

// removePasswordFile scrubs and deletes the password file. A failure is
// logged with the path only; it never blocks clearing the marker.
func (s *Server) removePasswordFile(path string) {
	if err := bootstrapcred.Remove(path); err != nil {
		log.Printf("WARNING: could not remove the initial admin password file %s: %v", path, err)
	}
}

// passwordPolicyMessage returns the client-facing reason a new admin password
// is refused, or "" when it is acceptable. Both password-change paths use it so
// the rule lives in one place: at least bootstrapcred.MinPasswordLength
// characters, and never the public default password "admin".
func passwordPolicyMessage(password string) string {
	if bootstrapcred.CheckPasswordPolicy(password) == nil {
		return ""
	}
	return fmt.Sprintf("password must be at least %d characters and must not be \"admin\"", bootstrapcred.MinPasswordLength)
}
