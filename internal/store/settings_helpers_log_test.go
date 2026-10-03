package store

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"
)

type failingSettingStore struct{ Store }

func (failingSettingStore) GetSetting(string) (string, error) { return "", errors.New("disk exploded") }

func TestGetJSONSettingLogsReadError(t *testing.T) {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	dst := []string{"keep"}
	GetJSONSetting[[]string](failingSettingStore{}, "k", &dst)

	if len(dst) != 1 || dst[0] != "keep" {
		t.Errorf("dst changed on read error: %v", dst)
	}
	if !strings.Contains(buf.String(), "disk exploded") {
		t.Errorf("read error not logged: %q", buf.String())
	}
}
