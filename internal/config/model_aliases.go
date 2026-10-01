package config

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// MaxModelAliasNameLen bounds both an alias name and its target. Model names
// in the wild (including "org/repo:tag" style names) are far shorter; the cap
// only exists so a stored alias can never grow without bound.
const MaxModelAliasNameLen = 256

// validateModelAliasName checks one side of an alias entry: non-empty, no
// surrounding whitespace, bounded length, and no control characters (CR/LF
// in particular would let an alias name split the X-Marbor-Model-Alias
// response header or a log line).
func validateModelAliasName(field, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("routing.model_aliases: %s must not be empty", field)
	}
	if strings.TrimSpace(name) != name {
		return fmt.Errorf("routing.model_aliases: %s %q must not have leading or trailing whitespace", field, name)
	}
	if len(name) > MaxModelAliasNameLen {
		return fmt.Errorf("routing.model_aliases: %s must be at most %d bytes", field, MaxModelAliasNameLen)
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return fmt.Errorf("routing.model_aliases: %s %q must not contain control characters", field, name)
		}
	}
	return nil
}

// ValidateModelAliasEntry validates a single alias -> target pair in
// isolation (shape rules only; the one-hop rule needs the whole map, see
// ValidateModelAliases).
func ValidateModelAliasEntry(alias, target string) error {
	if err := validateModelAliasName("alias", alias); err != nil {
		return err
	}
	if err := validateModelAliasName("target", target); err != nil {
		return err
	}
	if alias == target {
		return fmt.Errorf("routing.model_aliases: %q cannot point to itself", alias)
	}
	return nil
}

// ValidateModelAliases validates a whole alias map. Besides each entry's
// shape, it enforces the one-hop rule: no target may itself be an alias.
// That single check covers both directions (an alias whose target is another
// alias, and an alias name that is some other alias's target), so resolution
// can never chain or cycle. A nil or empty map is valid.
func ValidateModelAliases(aliases map[string]string) error {
	names := make([]string, 0, len(aliases))
	for alias := range aliases {
		names = append(names, alias)
	}
	sort.Strings(names)
	for _, alias := range names {
		target := aliases[alias]
		if err := ValidateModelAliasEntry(alias, target); err != nil {
			return err
		}
		if _, chained := aliases[target]; chained {
			return fmt.Errorf("routing.model_aliases: %q points to %q, which is itself an alias (aliases resolve one hop only)", alias, target)
		}
	}
	return nil
}

// SanitizeModelAliases returns a copy of aliases with every entry that would
// fail ValidateModelAliases removed, plus one human-readable reason per
// dropped entry. Used when loading persisted aliases at boot, so a stored
// entry that no longer passes (for example after the rules get stricter)
// is dropped and logged instead of making startup validation fatal. Returns
// nil when the input is empty.
func SanitizeModelAliases(aliases map[string]string) (map[string]string, []string) {
	if len(aliases) == 0 {
		return nil, nil
	}
	valid := make(map[string]string, len(aliases))
	var dropped []string
	names := make([]string, 0, len(aliases))
	for alias := range aliases {
		names = append(names, alias)
	}
	sort.Strings(names)
	for _, alias := range names {
		if err := ValidateModelAliasEntry(alias, aliases[alias]); err != nil {
			dropped = append(dropped, err.Error())
			continue
		}
		valid[alias] = aliases[alias]
	}
	out := make(map[string]string, len(valid))
	for _, alias := range names {
		target, ok := valid[alias]
		if !ok {
			continue
		}
		// An entry is kept only if its target is not another valid alias.
		// Any kept entry's target is therefore not a key of out either, so
		// the result satisfies the one-hop rule.
		if _, chained := valid[target]; chained {
			dropped = append(dropped, fmt.Sprintf("routing.model_aliases: %q points to %q, which is itself an alias (aliases resolve one hop only)", alias, target))
			continue
		}
		out[alias] = target
	}
	if len(out) == 0 {
		return nil, dropped
	}
	return out, dropped
}
