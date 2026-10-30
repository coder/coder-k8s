package storage

import "strings"

// lineTail keeps the newest lines of a log: at most maxLines lines and at most maxBytes bytes.
// Coder returns a build log from the start, so a tail request reads the log and keeps only its
// end. Memory stays bounded by maxBytes however large tailLines is.
type lineTail struct {
	maxLines int64
	maxBytes int64
	lines    []string // oldest first
	held     int64
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
		line := text[:end]
		if end < len(text) {
			// A held substring would keep the whole entry in memory, beyond maxBytes.
			line = strings.Clone(line)
		}
		t.pushLine(line)
		text = text[end:]
	}
}

func (t *lineTail) pushLine(line string) {
	if int64(len(line)) > t.maxBytes {
		line = strings.Clone(runePrefix(line, t.maxBytes))
		t.cut = true
	}
	if int64(len(t.lines)) == t.maxLines {
		t.dropOldest()
	}
	for t.held+int64(len(line)) > t.maxBytes {
		t.dropOldest()
		t.cut = true
	}
	t.lines = append(t.lines, line)
	t.held += int64(len(line))
}

func (t *lineTail) dropOldest() {
	t.held -= int64(len(t.lines[0]))
	t.lines[0] = "" // so the backing array does not keep the dropped line alive
	t.lines = t.lines[1:]
}

func (t *lineTail) String() string {
	var b strings.Builder
	b.Grow(int(t.held))
	for _, line := range t.lines {
		b.WriteString(line)
	}
	return b.String()
}
