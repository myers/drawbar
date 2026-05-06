package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
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

// runTail is the stub for the tail behavior. Implemented in later tasks.
func runTail(_ context.Context, _ tailArgs, _ io.Writer) error {
	return errors.New("not implemented")
}
