package store

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"
)

// captureLog redirects the standard logger into a buffer for the test and
// restores the previous writer when the test ends.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

// fakeSettingStore returns a fixed GetSetting result.
type fakeSettingStore struct {
	Store
	val string
	err error
}

func (f fakeSettingStore) GetSetting(string) (string, error) { return f.val, f.err }

func TestGetJSONSettingLogging(t *testing.T) {
	cases := []struct {
		name      string
		val       string
		err       error
		wantDst   string
		wantInLog string // empty means nothing may be logged
	}{
		{"read error is logged", "", errors.New("disk exploded"), "keep", "disk exploded"},
		{"empty value is silent", "", nil, "keep", ""},
		{"invalid JSON is logged", "{not json", nil, "keep", "GetJSONSetting"},
		{"valid value is returned silently", `["a","b"]`, nil, "a,b", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureLog(t)
			dst := []string{"keep"}
			GetJSONSetting[[]string](fakeSettingStore{val: tc.val, err: tc.err}, "k", &dst)

			if got := strings.Join(dst, ","); got != tc.wantDst {
				t.Errorf("dst = %q, want %q", got, tc.wantDst)
			}
			out := buf.String()
			if tc.wantInLog == "" && out != "" {
				t.Errorf("unexpected log: %q", out)
			}
			if tc.wantInLog != "" && !strings.Contains(out, tc.wantInLog) {
				t.Errorf("log %q missing %q", out, tc.wantInLog)
			}
		})
	}
}
