package main

import (
	"log"
	"time"

	"github.com/Anirudhx7/marbor/internal/auth"
	"github.com/Anirudhx7/marbor/internal/store"
)

// wireExpiryAudit records a system-audit event whenever an API key's stored
// expiry is found to be malformed. It must be called after the middleware has
// been built; keys already loaded are reported at that point. The same
// middleware object lives across config reloads, so the hook keeps working. A
// failed audit write is logged and never stops the process.
func wireExpiryAudit(mw *auth.Middleware, st store.Store) {
	mw.SetExpiryAuditHook(func(keyName string) {
		if err := st.AppendSystemAuditLog(store.SystemAuditEntry{
			Time:     time.Now().UTC(),
			Username: "system",
			Action:   auth.ExpiryAuditAction,
			Target:   keyName,
			Details:  auth.ExpiryAuditDetails,
			SourceIP: "",
		}); err != nil {
			log.Printf("auth: could not record malformed-expiry audit event for key %q: %v", keyName, err)
		}
	})
}
