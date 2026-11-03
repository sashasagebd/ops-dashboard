package engine

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
)

// LogLine is a line an app container wrote.
type LogLine struct {
	At     time.Time // when Docker received it
	Stream string    // "stdout" or "stderr"
	Line   string
}

// FollowLogs passes a container's output to emit line by line, starting with
// its last tail lines (or, if since isn't zero, everything after since), then
// following new output until ctx is done or the container stops, when it
// returns nil. Lines are cleaned up for showing (see cleanLine).
//
// Docker keeps each container's output itself (the json-file driver, capped
// by the hardening in Run), so the platform stores none of it.
func (c *Client) FollowLogs(ctx context.Context, id string, tail int, since time.Time, emit func(LogLine)) error {
	opts := client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
		Timestamps: true, // so each line has its time, and a reconnect can resume after it
		Tail:       strconv.Itoa(tail),
	}
	if !since.IsZero() {
		opts.Since = fmt.Sprintf("%d.%09d", since.Unix(), since.Nanosecond())
		opts.Tail = "all"
	}
	res, err := c.api.ContainerLogs(ctx, id, opts)
	if err != nil {
		return fmt.Errorf("reading container logs: %w", err)
	}
	defer res.Close()

	// App containers have no TTY, so Docker multiplexes stdout and stderr
	// into one stream with a small header per chunk; stdcopy separates them.
	writer := func(stream string) *lineWriter {
		return &lineWriter{emit: func(s string) { emit(parseLogLine(stream, s)) }}
	}
	stdout, stderr := writer("stdout"), writer("stderr")
	_, err = stdcopy.StdCopy(stdout, stderr, res)
	stdout.Flush()
	stderr.Flush()
	if ctx.Err() != nil {
		return nil // the caller stopped following
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("reading container logs: %w", err)
	}
	return nil
}

// parseLogLine splits the timestamp Docker puts first ("2026-11-03T10:00:00.123456789Z
// message") from the line.
func parseLogLine(stream, s string) LogLine {
	ts, line, _ := strings.Cut(s, " ")
	at, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		// Not timestamped (a line cut short by lineWriter's limit, say).
		at, line = time.Now(), s
	}
	return LogLine{At: at, Stream: stream, Line: cleanLine(line)}
}
