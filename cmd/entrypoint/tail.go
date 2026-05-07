package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"
)

// tailArgs holds parsed flags for the `entrypoint tail` subcommand.
type tailArgs struct {
	path string
	once bool
	skip int
}

// parseTailArgs parses the argv tail (everything after `tail`) into tailArgs.
// Flag forms recognized: --once, --skip N. Positional argument: path.
func parseTailArgs(argv []string) (tailArgs, error) {
	var args tailArgs
	i := 0
	for i < len(argv) {
		a := argv[i]
		switch a {
		case "--once":
			args.once = true
			i++
		case "--skip":
			if i+1 >= len(argv) {
				return tailArgs{}, errors.New("--skip requires a value")
			}
			n, err := strconv.Atoi(argv[i+1])
			if err != nil {
				return tailArgs{}, fmt.Errorf("--skip: %w", err)
			}
			if n < 0 {
				return tailArgs{}, errors.New("--skip must be non-negative")
			}
			args.skip = n
			i += 2
		default:
			if len(a) > 2 && a[:2] == "--" {
				return tailArgs{}, fmt.Errorf("unknown flag: %s", a)
			}
			if args.path != "" {
				return tailArgs{}, fmt.Errorf("unexpected argument: %s", a)
			}
			args.path = a
			i++
		}
	}
	if args.path == "" {
		return tailArgs{}, errors.New("missing path argument")
	}
	return args, nil
}

// runTail follows or one-shot reads the JSONL file at args.path, dropping the
// first args.skip newline-terminated lines and writing the remainder verbatim
// to out. Partial trailing lines (no \n) are never emitted.
//
// In follow mode (args.once == false) the function sleeps 50 ms on EOF and
// retries, so new lines appended to the file flow out as they arrive. It exits
// when ctx is cancelled, returning ctx.Err().
func runTail(ctx context.Context, args tailArgs, out io.Writer) error {
	f, err := os.Open(args.path)
	if err != nil {
		return fmt.Errorf("opening %s: %w", args.path, err)
	}
	defer f.Close()

	reader := bufio.NewReader(f)
	skipped := 0
	const followSleep = 50 * time.Millisecond

	// pending accumulates partial-line bytes across EOF-sleep cycles until a
	// '\n' arrives. bufio.Reader.ReadString consumes bytes into `line` when it
	// returns (partial, io.EOF) — those bytes no longer exist in the reader's
	// buffer, so we must carry them forward manually.
	var pending []byte

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line, rErr := reader.ReadString('\n')

		// Full line (rErr == nil, line ends in '\n').
		if rErr == nil && len(line) > 0 {
			full := line
			if len(pending) > 0 {
				full = string(pending) + line
				pending = nil
			}
			if skipped < args.skip {
				skipped++
				continue
			}
			if _, wErr := io.WriteString(out, full); wErr != nil {
				return fmt.Errorf("writing line: %w", wErr)
			}
			continue
		}

		// EOF — may include a partial line in `line`.
		if errors.Is(rErr, io.EOF) {
			if line != "" {
				// Partial bytes; carry them forward to be prepended once the
				// line is completed by a future write.
				pending = append(pending, line...)
			}
			if args.once {
				// --once mode: discard any pending partial line and return.
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(followSleep):
			}
			continue
		}

		return fmt.Errorf("reading: %w", rErr)
	}
}
