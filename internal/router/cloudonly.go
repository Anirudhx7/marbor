package router

import (
	"log"
	"strings"
)

// cloudOnlySnapshot is an immutable, pre-folded view of the cloud-only models
// list. It is built once per change and published with one atomic swap, so the
// per-request check takes no lock.
type cloudOnlySnapshot struct {
	// entries is the list as the operator wrote it, for read-back.
	entries []string
	// exact holds the lower-cased exact-name entries.
	exact []string
	// prefixes holds the lower-cased prefix entries with the trailing "*" removed.
	prefixes []string
}

// SetCloudOnlyModels replaces the cloud-only models list. The input is copied
// and the copy is published with a single atomic swap, so a concurrent
// IsCloudOnly sees either the complete old list or the complete new one.
// Entries are exact model names or a name ending in one "*" (prefix); both
// forms are matched case-insensitively. Callers pass an already-validated
// list; an entry that is empty or a bare "*" is ignored here (and left out of
// the read-back) rather than treated as a wildcard. nil or empty clears the
// list.
func (r *Router) SetCloudOnlyModels(list []string) {
	// A valid list replaces whatever state the stored row left behind. The new
	// list is published before the unreadable state is lifted, so there is no
	// moment where the state is lifted but the old, empty list is still live.
	if len(list) == 0 {
		r.cloudOnlyModels.Store(nil)
		r.cloudOnlyUnreadable.Store(false)
		return
	}
	snap := &cloudOnlySnapshot{entries: make([]string, 0, len(list))}
	for _, entry := range list {
		folded := strings.ToLower(entry)
		// An empty entry or a bare "*" is dropped from the read-back as well as
		// from matching, so the list shown is the list enforced.
		if folded == "" || folded == "*" {
			continue
		}
		snap.entries = append(snap.entries, entry)
		if prefix, isPrefix := strings.CutSuffix(folded, "*"); isPrefix {
			snap.prefixes = append(snap.prefixes, prefix)
			continue
		}
		snap.exact = append(snap.exact, folded)
	}
	r.cloudOnlyModels.Store(snap)
	r.cloudOnlyUnreadable.Store(false)
}

// SetCloudOnlyUnreadable marks the stored list as unreadable: the operator's
// list is unknown, so IsCloudOnly answers true for every named model and the
// proxy refuses those requests instead of running a possibly listed model
// locally. Saving a valid list (SetCloudOnlyModels) lifts it.
func (r *Router) SetCloudOnlyUnreadable() {
	r.cloudOnlyUnreadable.Store(true)
}

// CloudOnlyUnreadable reports whether the stored list is currently unreadable.
func (r *Router) CloudOnlyUnreadable() bool {
	return r.cloudOnlyUnreadable.Load()
}

// CloudOnlyModels returns a fresh copy of the current list in the operator's
// order (never nil).
func (r *Router) CloudOnlyModels() []string {
	out := []string{}
	if s := r.cloudOnlyModels.Load(); s != nil {
		out = append(out, s.entries...)
	}
	return out
}

// IsCloudOnly reports whether any of names is on the cloud-only list. An exact
// entry matches a name under ModelNamesEquivalent (so "llama3" also matches
// "llama3:latest"), a "prefix*" entry matches a name that starts with the
// prefix, and both ignore case. An empty name never matches. The two forms
// differ on tags: an exact entry matches the name and its ":latest" spelling
// only ("llama3" does not cover "llama3:8b"), while a prefix entry compares
// the raw name, so "llama3*" covers "llama3:8b" because that name starts with
// "llama3".
func (r *Router) IsCloudOnly(names ...string) bool {
	if r.cloudOnlyUnreadable.Load() {
		// The list is unknown, so any named model may be on it: fail closed.
		for _, name := range names {
			if name != "" {
				return true
			}
		}
		return false
	}
	s := r.cloudOnlyModels.Load()
	if s == nil {
		return false
	}
	for _, name := range names {
		if name == "" {
			continue
		}
		lower := strings.ToLower(name)
		for _, e := range s.exact {
			if ModelNamesEquivalent(e, lower) {
				return true
			}
		}
		for _, p := range s.prefixes {
			if strings.HasPrefix(lower, p) {
				return true
			}
		}
	}
	return false
}

// CloudOnlyLabel returns the lower-cased exact list entry that name matches,
// or "" when none does. A metric label uses it so that only a name the
// operator declared verbatim gets its own label, and every case or tag variant
// of it shares that one label; a name caught only by a prefix entry is
// client-chosen and must not mint a series.
func (r *Router) CloudOnlyLabel(name string) string {
	s := r.cloudOnlyModels.Load()
	if s == nil || name == "" {
		return ""
	}
	lower := strings.ToLower(name)
	for _, e := range s.exact {
		if ModelNamesEquivalent(e, lower) {
			return e
		}
	}
	return ""
}

// skipCloudOnlyPrewarm reports whether an automatic loader (predictive
// pre-warm, the keep-warm pinger, a scheduled warmup) must leave model alone
// because it is on the cloud-only list. Each skip logs one rate-limited line.
// It only filters at decision time: stored history and warmup lists are not
// touched, so removing an entry from the list restores the old behavior.
func (r *Router) skipCloudOnlyPrewarm(node, model string) bool {
	if !r.IsCloudOnly(model) {
		return false
	}
	if r.allowHostLog("cloudonly:" + node + ":" + model) {
		if r.cloudOnlyUnreadable.Load() {
			log.Printf("warmup skipped on node %q for model %q: the stored cloud-only models list is unreadable, so no model is loaded automatically until it is restored", node, truncateForLog(model, 64))
		} else {
			log.Printf("warmup skipped on node %q: model %q is on the cloud-only list and is never loaded locally", node, truncateForLog(model, 64))
		}
	}
	return true
}
