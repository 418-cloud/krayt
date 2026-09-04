package orchestrator

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// TestElideMiddleKeepsBothEnds pins the difference between this cap and writeConsoleLog's. The
// system log keeps its tail because a boot failure is the last thing in it; a transcript has to
// keep BOTH ends, because the question it answers ("when did this first appear?") lives at the
// front and the failure lives at the back. A tail-only truncation would have thrown away exactly
// the evidence this artifact exists to preserve.
func TestElideMiddleKeepsBothEnds(t *testing.T) {
	var b bytes.Buffer
	for i := 0; i < 20000; i++ {
		fmt.Fprintf(&b, `{"line":%d,"filler":"%s"}`+"\n", i, strings.Repeat("x", 200))
	}
	orig := b.Bytes()
	const max, head = 1 << 14, 1 << 12
	got := elideMiddle(orig, max, head)

	if len(got) > max+128 { // +marker
		t.Errorf("elideMiddle returned %d bytes, want <= %d plus the marker", len(got), max)
	}
	if !bytes.Contains(got, []byte(`{"line":0,`)) {
		t.Error("the head was dropped; the first event in the transcript is the one being looked for")
	}
	if !bytes.Contains(got, []byte(`{"line":19999,`)) {
		t.Error("the tail was dropped; the failure is at the end of a transcript")
	}
	if !bytes.Contains(got, []byte("krayt elided")) {
		t.Error("no elision marker: a reader cannot tell a truncated transcript from a complete one")
	}
	// Cut on line boundaries, or a half-line of JSON reads as corruption rather than truncation.
	for _, line := range bytes.Split(got, []byte("\n")) {
		if len(line) == 0 || bytes.HasPrefix(line, []byte("...")) {
			continue
		}
		if !bytes.HasPrefix(line, []byte("{")) || !bytes.HasSuffix(line, []byte("}")) {
			t.Errorf("line cut mid-record: %.60q", line)
			break
		}
	}
}

func TestElideMiddleLeavesSmallInputAlone(t *testing.T) {
	in := []byte("{\"a\":1}\n{\"b\":2}\n")
	if got := elideMiddle(in, 1<<20, 1<<10); !bytes.Equal(got, in) {
		t.Errorf("elideMiddle rewrote an input that already fits: %q", got)
	}
}
