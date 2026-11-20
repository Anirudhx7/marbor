package store

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"
)

// The first rejected split is always logged, then only one in every
// rejectedSplitLogEvery, and the line carries the running count.
func TestNoteRejectedCacheSplitLogsFirstThenEveryNth(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	rejectedSplits.Store(0)
	t.Cleanup(func() { rejectedSplits.Store(0) })

	in, ca := int64(5), int64(6)
	NoteRejectedCacheSplit("unit", &in, &ca)
	first := buf.String()
	if !strings.Contains(first, "unit dropped an invalid cache split (input=5 cached=6); 1 dropped so far") {
		t.Fatalf("first drop line = %q", first)
	}
	buf.Reset()
	for i := 2; i < rejectedSplitLogEvery; i++ {
		NoteRejectedCacheSplit("unit", &in, &ca)
	}
	if buf.Len() != 0 {
		t.Fatalf("drops 2..%d logged: %q", rejectedSplitLogEvery-1, buf.String())
	}
	NoteRejectedCacheSplit("unit", &in, nil)
	if !strings.Contains(buf.String(), "cached=unset); 1000 dropped so far") {
		t.Errorf("drop %d line = %q", rejectedSplitLogEvery, buf.String())
	}
}
