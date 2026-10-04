package admin

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/Anirudhx7/marbor/internal/bootstrapcred"
	"github.com/Anirudhx7/marbor/internal/store"
)

// SetAdminBindOverride records that the admin bind address was forced from the
// environment for this process. effective is the forced address; stored is the
// value held in the database (or its default). The server keeps reporting and
// saving the STORED value, so a dashboard or CLI round trip through the
// settings never turns the temporary override into the saved bind, and an
// operator who deliberately saves a different stored bind while the override
// is active has exactly that value persisted.
func (s *Server) SetAdminBindOverride(effective, stored string) {
	s.boot.bindOverride = effective
	if effective == "" {
		return
	}
	s.mu.Lock()
	s.cfg.Admin.BindAddress = stored
	s.mu.Unlock()
}

// demoActive reports whether this is an explicitly flagged demo deployment.
// Being bound to a local address never counts.
func (s *Server) demoActive() bool {
	return s.demoMode || bootstrapcred.EnvTrue(s.boot.getenv, bootstrapcred.EnvDemoMode)
}

// demoExempt reports whether the demo flag really covers the pending credential:
// the pending admin must actually verify against the public default password
// (demo stacks ship that login on purpose) and the origin must not be fresh. A
// freshly generated or operator-supplied credential is never public, so the
// demo flag cannot be used to expose it on a non-loopback bind.
func (s *Server) demoExempt(origin bootstrapcred.Origin) bool {
	if origin == bootstrapcred.OriginFresh {
		return false
	}
	defaults, err := s.activeDefaultAdmins()
	if err != nil {
		log.Printf("ERROR: demo exemption not applied: %v", err)
		return false
	}
	return len(defaults) > 0
}

// pendingOrigin returns the origin of the active initial/default password. A
// default-password admin with no marker (created after boot) is enforced like a
// fresh credential. An unreadable marker is an error: the caller fails closed.
func (s *Server) pendingOrigin() (bootstrapcred.Origin, bool, error) {
	if !s.boot.isPending() {
		return "", false, nil
	}
	m, err := s.loadMarker()
	if err != nil {
		return "", true, err
	}
	if m != nil {
		return m.Origin, true, nil
	}
	return bootstrapcred.OriginFresh, true, nil
}

// AdminListenerGate decides whether the already-bound admin listener may serve.
// A nil result means serve. A non-nil result is the actionable reason the admin
// dashboard must not start; the caller closes the listener and keeps the proxy
// and metrics running. configured is the bind string the listener was opened
// from. The decision uses the listener's real bound address, never the string.
func (s *Server) AdminListenerGate(ln net.Listener, configured string) error {
	if s.boot.err != nil {
		return fmt.Errorf("admin dashboard not started: the first-boot administrator credential is not usable: %w", s.boot.err)
	}
	origin, pending, err := s.pendingOrigin()
	if err != nil {
		return fmt.Errorf("admin dashboard not started: the first-boot marker could not be read: %w", err)
	}
	if !pending {
		return nil
	}
	if s.demoActive() && s.demoExempt(origin) {
		s.warnDemoExemption(configured)
		return nil
	}
	if origin == bootstrapcred.OriginUpgraded && s.boot.graceActive(s.version) {
		log.Printf("WARNING: the administrator is still on the default password. This release only warns; the next release refuses to start the admin dashboard on a non-loopback bind until the password is changed")
		return nil
	}
	loopback, reason := bootstrapcred.BindIsLoopback(ln.Addr(), configured)
	if loopback {
		return nil
	}
	if bootstrapcred.EnvTrue(s.boot.getenv, bootstrapcred.EnvAllowInsecure) {
		s.warnInsecureOverride(configured, reason)
		return nil
	}
	return s.refusalError(reason)
}

// adminBindWarning returns a save-time warning when persisting bind would make
// the next restart refuse to start the admin dashboard: an initial/default admin
// password is still active, the bind is not loopback, and none of the documented
// exceptions (the insecure escape hatch, the demo flag on a public default, the
// one-release upgrade grace) applies. It is "" when the save is safe. It only
// reads state; the gate's audit entries are written by the gate itself at start.
func (s *Server) adminBindWarning(bind string) string {
	if !s.boot.isPending() || bootstrapcred.ConfiguredBindIsLoopback(bind) {
		return ""
	}
	if origin, _, err := s.pendingOrigin(); err == nil {
		if s.demoActive() && s.demoExempt(origin) {
			return ""
		}
		if origin == bootstrapcred.OriginUpgraded && s.boot.graceActive(s.version) {
			return ""
		}
	}
	if bootstrapcred.EnvTrue(s.boot.getenv, bootstrapcred.EnvAllowInsecure) {
		return ""
	}
	return fmt.Sprintf("The admin bind address %q is not loopback while the initial admin password is still active. "+
		"After the next restart the admin dashboard will not start there and you will be locked out until you use %s=127.0.0.1:8080. "+
		"Change the admin password first, or keep the bind on a loopback address.", bind, bootstrapcred.EnvAdminBind)
}

// refusalError builds the actionable refusal text. It never contains a
// password; it may name the password file path.
func (s *Server) refusalError(reason string) error {
	where := ""
	if s.boot.dir != "" {
		where = fmt.Sprintf(" A generated initial password, if one was created, is in %s.", bootstrapcred.FilePath(s.boot.dir))
	}
	return fmt.Errorf("admin dashboard not started: initial admin password still active and the admin bind address is not loopback (%s). "+
		"Bind it to a loopback address (set %s=127.0.0.1:8080 and restart), log in, change the password, then restore the bind. "+
		"New installs can instead supply the bootstrap password at first boot with %s (preferred) or %s (weaker).%s",
		reason, bootstrapcred.EnvAdminBind, bootstrapcred.EnvPasswordFile, bootstrapcred.EnvPassword, where)
}

// auditInsecureAdmin records, in the system audit log, that the admin listener
// was allowed to serve on a non-loopback bind with an initial/default password
// still active. A failed write is logged loudly: the exposure is real even when
// the audit store is not.
func (s *Server) auditInsecureAdmin(configured, details string) {
	err := s.st.AppendSystemAuditLog(store.SystemAuditEntry{
		Time:     time.Now(),
		Username: "system",
		Action:   "insecure_default_admin_override",
		Target:   configured,
		Details:  details,
	})
	if err != nil {
		log.Printf("ERROR: could not write the system audit entry for the insecure admin listener on %q: %v", configured, err)
	}
}

// warnInsecureOverride logs the escape hatch on every boot it is in effect and
// records a system audit entry.
func (s *Server) warnInsecureOverride(configured, reason string) {
	log.Printf("WARNING: %s=true is in effect: the admin dashboard is served on %q (%s) while an initial/default admin password is still active. "+
		"Anyone who can reach it over plaintext HTTP can take over the control plane. Change the password and remove this setting.",
		bootstrapcred.EnvAllowInsecure, configured, reason)
	s.auditInsecureAdmin(configured, bootstrapcred.EnvAllowInsecure+"=true allowed the admin listener with an initial/default password active: "+reason)
}

// warnDemoExemption logs, loudly, that the demo flag let the admin listener
// serve with the public default password, and records the same audit entry as
// the escape hatch.
func (s *Server) warnDemoExemption(configured string) {
	log.Printf("WARNING: demo mode is on (%s=true or the demo server flag): the admin dashboard is served on %q with the PUBLIC default admin password. "+
		"Anyone who can reach it can log in. Never use demo mode on a real deployment.", bootstrapcred.EnvDemoMode, configured)
	s.auditInsecureAdmin(configured, "demo mode allowed the admin listener with the public default admin password active")
}

// proxyHeaders are the request headers a reverse proxy adds. A request carrying
// any of them was relayed, so its TCP peer is the proxy, not the real caller.
var proxyHeaders = []string{"X-Forwarded-For", "Forwarded", "X-Real-IP", "CF-Connecting-IP", "True-Client-IP", "X-Client-IP", "Via", "X-Forwarded-Host", "Forwarded-Host"}

// isLoopbackClient reports whether the caller of r is local: the TCP peer is a
// loopback address AND the request carries no proxy header. A reverse proxy on
// the same host connects from loopback for every remote caller, so its headers
// are not trusted to identify the caller (they could be forged or absent); their
// mere presence marks the request as relayed and therefore not local. A proxy
// that strips its headers cannot be told apart from a local caller, which is why
// the deployment guide says a same-host proxy exposes the login by design.
func isLoopbackClient(r *http.Request) bool {
	for _, h := range proxyHeaders {
		if len(r.Header.Values(h)) > 0 {
			return false
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// BootstrapLoginHint is one line for the startup banner that tells the operator
// how to sign in for the first time. It names where the secret comes from (a
// file path or an environment variable), never the secret itself.
func (s *Server) BootstrapLoginHint() string {
	if s.boot.err != nil {
		return "unavailable until the first-boot credential problem in the log is fixed"
	}
	m, err := s.loadMarker()
	if err != nil {
		return "unavailable until the first-boot marker problem in the log is fixed"
	}
	switch {
	case m != nil && m.Origin == bootstrapcred.OriginFresh:
		return s.freshLoginHint(*m)
	case m != nil:
		return fmt.Sprintf("user %q with the public default password (a password change is required at first login)", m.Username)
	case s.boot.isPending():
		return "admin / admin (a password change is required at first login)"
	}
	return "your administrator account"
}

// freshLoginHint names the real source of a fresh install's initial password.
func (s *Server) freshLoginHint(m bootstrapcred.Marker) string {
	username := m.Username
	if s.boot.dir != "" && !m.Supplied {
		path := bootstrapcred.FilePath(s.boot.dir)
		if ok, _ := bootstrapcred.Exists(path); ok {
			return fmt.Sprintf("user %q, initial password in the file %s (removed after the first password change)", username, path)
		}
	}
	switch {
	case s.boot.source == bootstrapcred.SourceFile || s.boot.getenv(bootstrapcred.EnvPasswordFile) != "":
		return fmt.Sprintf("user %q, with the password in the file named by %s", username, bootstrapcred.EnvPasswordFile)
	case s.boot.source == bootstrapcred.SourceEnv || s.boot.envSecretSeen:
		return fmt.Sprintf("user %q, with the password you set in %s", username, bootstrapcred.EnvPassword)
	}
	return fmt.Sprintf("user %q, with the initial password supplied at first boot", username)
}
