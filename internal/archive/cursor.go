package archive

import (
	"fmt"
	"strconv"
	"strings"
)

// Cursor is a position in the order of a chat's messages, which is (ts, id).
// ts is in seconds and shared by neighbours, so a cursor of the time alone
// would lose or repeat messages at the edge of a page; id, the order they were
// stored in, tells them apart. The zero Cursor is no position at all.
//
// A time t is the cursor {TS: t.Unix()}: ids start at 1, so it stands before
// every message of that second and after every message of the earlier ones.
type Cursor struct{ TS, ID int64 }

func (c Cursor) IsZero() bool { return c == Cursor{} }

// String is "<ts>_<id>": a tool hands it to the model as an opaque token. Not
// a '.', which would read as a fractional unix time. The zero Cursor is "": it
// is no position, and a token for it ("0_0") that came back as the start of
// the chat would send a caller that forgot to check Page.Next round and round
// the newest page.
func (c Cursor) String() string {
	if c.IsZero() {
		return ""
	}
	return strconv.FormatInt(c.TS, 10) + "_" + strconv.FormatInt(c.ID, 10)
}

// ParseCursor is the reverse of String. The zero Cursor has no spelling, so
// "" and "0_0" are errors.
func ParseCursor(s string) (Cursor, error) {
	ts, id, ok := strings.Cut(s, "_")
	t, errT := strconv.ParseUint(ts, 10, 63) // no sign, no underscores: one spelling for one cursor
	i, errI := strconv.ParseUint(id, 10, 63)
	if !ok || errT != nil || errI != nil || (t == 0 && i == 0) {
		return Cursor{}, fmt.Errorf("invalid cursor %q: want <ts>_<id>", s)
	}
	return Cursor{int64(t), int64(i)}, nil
}
