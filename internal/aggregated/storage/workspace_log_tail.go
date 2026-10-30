package storage

import (
	"bytes"
	"strings"
)

// lineTail keeps the newest lines of a log: at most maxLines lines and at most maxBytes bytes.
// Coder returns a build log from the start, so a tail request reads the log and keeps only its
// end. The lines share one byte buffer of at most 2*maxBytes, with no allocation per line, so
// memory stays bounded however large tailLines is and however many line breaks an entry has.
type lineTail struct {
	maxLines int64
	maxBytes int64
	buf      []byte // the held lines are buf[start:], each ending with '\n'
	start    int
	lines    int64
	// cut is set when the byte cap dropped or shortened a line that maxLines would keep.
	cut bool
}

func newLineTail(maxLines, maxBytes int64) *lineTail {
	if maxLines < 0 || maxBytes < 1 {
		panic("assertion failed: a log tail needs maxLines >= 0 and maxBytes >= 1")
	}
	return &lineTail{maxLines: maxLines, maxBytes: maxBytes}
}

// push adds text that ends with a newline. Each newline ends one line, as in kubectl logs --tail,
// so an entry whose output has line breaks counts as several lines.
func (t *lineTail) push(text string) {
	if t.maxLines == 0 {
		return
	}
	for text != "" {
		end := strings.IndexByte(text, '\n') + 1
		if end == 0 {
			panic("assertion failed: log tail text must end with a newline")
		}
		t.pushLine(text[:end])
		text = text[end:]
	}
}

func (t *lineTail) pushLine(line string) {
	if int64(len(line)) > t.maxBytes {
		// Keep the start of the line and its line end, so every held line ends with '\n'.
		line = runePrefix(line, t.maxBytes-1) + "\n"
		t.cut = true
	}
	if t.lines == t.maxLines {
		t.dropOldest()
	}
	for t.held()+int64(len(line)) > t.maxBytes {
		t.dropOldest()
		t.cut = true
	}
	if len(t.buf)+len(line) > cap(t.buf) {
		live := t.buf[t.start:]
		if t.start >= len(live) {
			// The dropped bytes pay for this copy, so compaction is amortized O(1) per byte.
			t.buf = t.buf[:copy(t.buf, live)]
		} else {
			grown := make([]byte, len(live), min(2*(int64(len(live))+int64(len(line))), 2*t.maxBytes))
			copy(grown, live)
			t.buf = grown
		}
		t.start = 0
	}
	t.buf = append(t.buf, line...)
	t.lines++
}

func (t *lineTail) held() int64 { return int64(len(t.buf) - t.start) }

func (t *lineTail) dropOldest() {
	t.start += bytes.IndexByte(t.buf[t.start:], '\n') + 1
	t.lines--
}

func (t *lineTail) String() string { return string(t.buf[t.start:]) }
