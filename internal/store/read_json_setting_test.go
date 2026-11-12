package store

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestReadJSONSetting(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got := []string{"keep"}
	if err := ReadJSONSetting(st, "k", &got); err != nil || !reflect.DeepEqual(got, []string{"keep"}) {
		t.Fatalf("absent row: err=%v got=%v, want nil error and dst untouched", err, got)
	}
	_ = st.SetSetting("k", "")
	if err := ReadJSONSetting(st, "k", &got); err != nil || !reflect.DeepEqual(got, []string{"keep"}) {
		t.Fatalf("empty row: err=%v got=%v", err, got)
	}
	_ = st.SetSetting("k", `{bad`)
	if err := ReadJSONSetting(st, "k", &got); err == nil || !reflect.DeepEqual(got, []string{"keep"}) {
		t.Fatalf("invalid json: err=%v got=%v, want an error and dst untouched", err, got)
	}
	_ = st.SetSetting("k", `["a","b"]`)
	if err := ReadJSONSetting(st, "k", &got); err != nil || !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("valid: err=%v got=%v", err, got)
	}
}
