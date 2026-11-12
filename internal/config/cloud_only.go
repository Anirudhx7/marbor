package config

import (
	"fmt"
	"strings"
	"unicode"
)

// MaxCloudOnlyModels caps how many entries the cloud-only models list may
// hold. It is far above any real list and keeps the per-request scan trivial.
const MaxCloudOnlyModels = 200

// validateCloudOnlyEntry checks one list entry: a model name, or a name with a
// single trailing "*" that matches by prefix. An exact name matches itself and
// its ":latest" spelling only ("llama3" does not cover "llama3:8b"), while a
// prefix is compared against the name as sent, tag included. The bare "*" is
// rejected because it would switch local routing off for every model, which is
// a different feature from naming specific models.
func validateCloudOnlyEntry(entry string) error {
	if strings.TrimSpace(entry) == "" {
		return fmt.Errorf("routing.cloud_only_models: entries must not be empty")
	}
	if len(entry) > MaxModelAliasNameLen {
		return fmt.Errorf("routing.cloud_only_models: entry must be at most %d bytes", MaxModelAliasNameLen)
	}
	for _, r := range entry {
		if unicode.IsSpace(r) {
			return fmt.Errorf("routing.cloud_only_models: entry %q must not contain whitespace", entry)
		}
		if unicode.IsControl(r) {
			return fmt.Errorf("routing.cloud_only_models: entry %q must not contain control characters", entry)
		}
	}
	if entry == "*" {
		return fmt.Errorf("routing.cloud_only_models: a bare \"*\" is not allowed; list model names or a prefix such as \"family-*\"")
	}
	if i := strings.Index(entry, "*"); i >= 0 && i != len(entry)-1 {
		return fmt.Errorf("routing.cloud_only_models: entry %q may use \"*\" only as its last character", entry)
	}
	return nil
}

// ValidateCloudOnlyModels validates the whole cloud-only models list: every
// entry's shape, no duplicates (compared case-insensitively, because the
// matcher folds case), and at most MaxCloudOnlyModels entries. A nil or empty
// list is valid.
func ValidateCloudOnlyModels(list []string) error {
	if len(list) > MaxCloudOnlyModels {
		return fmt.Errorf("routing.cloud_only_models: at most %d entries are allowed", MaxCloudOnlyModels)
	}
	seen := make(map[string]struct{}, len(list))
	for _, entry := range list {
		if err := validateCloudOnlyEntry(entry); err != nil {
			return err
		}
		key := strings.ToLower(entry)
		if _, dup := seen[key]; dup {
			return fmt.Errorf("routing.cloud_only_models: %q is listed more than once", entry)
		}
		seen[key] = struct{}{}
	}
	return nil
}

// CloudOnlySanitized is the result of SanitizeCloudOnlyModels: the entries
// kept, and one reason per entry dropped.
type CloudOnlySanitized struct {
	Kept    []string
	Dropped []string
}

// SanitizeCloudOnlyModels returns a copy of list with every entry that would
// fail ValidateCloudOnlyModels removed (bad shape, duplicate, or beyond the
// count cap), keeping the operator's order, plus one reason per dropped entry.
// Used when loading the persisted list at boot so a stored entry that no
// longer passes is dropped and logged instead of making startup validation
// fatal. Returns nil when nothing is kept.
func SanitizeCloudOnlyModels(list []string) CloudOnlySanitized {
	if len(list) == 0 {
		return CloudOnlySanitized{}
	}
	var dropped []string
	out := make([]string, 0, len(list))
	seen := make(map[string]struct{}, len(list))
	for _, entry := range list {
		if err := validateCloudOnlyEntry(entry); err != nil {
			dropped = append(dropped, err.Error())
			continue
		}
		key := strings.ToLower(entry)
		if _, dup := seen[key]; dup {
			dropped = append(dropped, fmt.Sprintf("routing.cloud_only_models: %q is listed more than once", entry))
			continue
		}
		if len(out) >= MaxCloudOnlyModels {
			dropped = append(dropped, fmt.Sprintf("routing.cloud_only_models: %q is beyond the %d-entry limit", entry, MaxCloudOnlyModels))
			continue
		}
		seen[key] = struct{}{}
		out = append(out, entry)
	}
	if len(out) == 0 {
		return CloudOnlySanitized{Dropped: dropped}
	}
	return CloudOnlySanitized{Kept: out, Dropped: dropped}
}
