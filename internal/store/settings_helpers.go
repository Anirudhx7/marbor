package store

import (
	"encoding/json"
	"log"
	"strconv"
)

// This file provides small typed helpers over the generic Store.GetSetting/
// SetSetting string KV so main.go's boot-time settings overlay and admin.go's
// handleUpdateSettings/handleSettings don't each hand-roll string<->typed
// conversions for every one of the ~35 settings-backed Config fields
// (2026-07 config.yaml elimination). Every helper degrades to a caller-
// supplied default on a missing key, parse failure, or store error - never
// panics, never returns an error the caller must handle.

// GetStringSetting returns the stored value for key, or def if absent/empty.
func GetStringSetting(st Store, key, def string) string {
	if v, err := st.GetSetting(key); err == nil && v != "" {
		return v
	}
	return def
}

// GetBoolSetting returns the stored "true"/"false" value for key, or def if
// absent/empty.
func GetBoolSetting(st Store, key string, def bool) bool {
	if v, err := st.GetSetting(key); err == nil && v != "" {
		if b, convErr := strconv.ParseBool(v); convErr == nil {
			return b
		}
	}
	return def
}

// GetIntSetting returns the stored integer value for key, or def if
// absent/empty/unparseable.
func GetIntSetting(st Store, key string, def int) int {
	if v, err := st.GetSetting(key); err == nil && v != "" {
		if n, convErr := strconv.Atoi(v); convErr == nil {
			return n
		}
	}
	return def
}

// GetFloatSetting returns the stored float value for key, or def if
// absent/empty/unparseable.
func GetFloatSetting(st Store, key string, def float64) float64 {
	if v, err := st.GetSetting(key); err == nil && v != "" {
		if n, convErr := strconv.ParseFloat(v, 64); convErr == nil {
			return n
		}
	}
	return def
}

// GetJSONSetting unmarshals the stored JSON value for key into dst, leaving
// dst untouched (caller's zero-value/default) if the key is absent/empty/
// invalid JSON.
func GetJSONSetting[T any](st Store, key string, dst *T) {
	v, err := st.GetSetting(key)
	if err != nil || v == "" {
		return
	}
	var tmp T
	if err := json.Unmarshal([]byte(v), &tmp); err != nil {
		// encoding/json populates fields as it parses before erroring on
		// malformed trailing content, so unmarshaling directly into dst
		// could leave it partially mutated - contradicting this function's
		// own doc comment promise that dst stays untouched on invalid JSON.
		log.Printf("store: GetJSONSetting(%q): invalid JSON: %v", key, err)
		return
	}
	*dst = tmp
}

// SetJSONSetting marshals v and persists it under key. Returns the
// marshal/store error, if any, for the caller to log.
func SetJSONSetting(st Store, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return st.SetSetting(key, string(b))
}
