package main

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

// A stored cloud-only row that cannot be read or parsed does not stop startup,
// but the router starts failing closed; a valid, null, empty or absent row does
// not, and neither does a list whose entries are all invalid.
func TestApplyPersistedSettings_CloudOnlyRow(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		set        bool
		unreadable bool
		want       []string
	}{
		{name: "absent row"},
		{name: "empty row", set: true},
		{name: "valid list", raw: `["a","b*"]`, set: true, want: []string{"a", "b*"}},
		{name: "invalid json", raw: `{not json`, set: true, unreadable: true},
		{name: "wrong shape", raw: `{"a":1}`, set: true, unreadable: true},
		{name: "null row", raw: "null", set: true},
		{name: "every entry invalid", raw: `["*","bad name"]`, set: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if tc.set {
				if err := st.SetSetting("routing_cloud_only_models", tc.raw); err != nil {
					t.Fatal(err)
				}
			}
			cfg := &config.Config{}
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			applyPersistedSettings(cfg, st)
			if err := cfg.Validate(); err != nil {
				t.Fatalf("startup validation must not fail on a bad row: %v", err)
			}
			if cfg.Routing.CloudOnlyListUnreadable != tc.unreadable {
				t.Fatalf("unreadable = %v, want %v", cfg.Routing.CloudOnlyListUnreadable, tc.unreadable)
			}
			if len(tc.want) == 0 && len(cfg.Routing.CloudOnlyModels) != 0 {
				t.Fatalf("list = %v, want empty", cfg.Routing.CloudOnlyModels)
			}
			if len(tc.want) > 0 && !reflect.DeepEqual(cfg.Routing.CloudOnlyModels, tc.want) {
				t.Fatalf("list = %v, want %v", cfg.Routing.CloudOnlyModels, tc.want)
			}
			r := router.New(cfg.Routing, nil, nil)
			if got := r.IsCloudOnly("some-model"); got != tc.unreadable {
				t.Fatalf("router fail-closed = %v, want %v", got, tc.unreadable)
			}
		})
	}
}

type failingCloudOnlyRead struct {
	store.Store
}

func (f failingCloudOnlyRead) GetSetting(key string) (string, error) {
	if key == "routing_cloud_only_models" {
		return "", fmt.Errorf("injected read failure")
	}
	return f.Store.GetSetting(key)
}

// A store read error (not just bad JSON) also fails closed.
func TestApplyPersistedSettings_CloudOnlyReadError(t *testing.T) {
	inner, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer inner.Close()
	cfg := &config.Config{}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	applyPersistedSettings(cfg, failingCloudOnlyRead{inner})
	if !cfg.Routing.CloudOnlyListUnreadable {
		t.Fatal("a failed read of the cloud-only row must set the unreadable state")
	}
	if len(cfg.Routing.CloudOnlyModels) != 0 {
		t.Fatalf("list = %v, want empty", cfg.Routing.CloudOnlyModels)
	}
	if !router.New(cfg.Routing, nil, nil).IsCloudOnly("some-model") {
		t.Fatal("the router built from this config must fail closed")
	}
}
