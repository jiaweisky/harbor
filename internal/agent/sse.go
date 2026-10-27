package agent

import (
	"bufio"
	"io"
	"strings"
)

// ReadSSE reads server-sent events from r and calls fn for each one with its
// event name (empty for the default message event) and its joined data payload.
// A line beginning with ':' is a comment. Reading stops when fn returns false,
// and ReadSSE returns the underlying read error, if any.
func ReadSSE(r io.Reader, fn func(event, data string) bool) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var event string
	var data []string

	dispatch := func() bool {
		if event == "" && len(data) == 0 {
			return true
		}
		ok := fn(event, strings.Join(data, "\n"))
		event, data = "", nil
		return ok
	}

	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if !dispatch() {
				return sc.Err()
			}
		case strings.HasPrefix(line, ":"):
			// Comment line.
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}

	dispatch()

	return sc.Err()
}
