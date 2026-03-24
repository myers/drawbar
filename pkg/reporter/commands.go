package reporter

import (
	"regexp"
	"strings"
)

// cmdRegex matches GitHub Actions workflow commands: ::command properties::value
var cmdRegex = regexp.MustCompile(`^::([^ :]+)( .*)?::(.*)$`)

// CommandProcessor parses GitHub Actions workflow commands from log lines.
// It handles ::add-mask::, ::debug::, ::group::, ::stop-commands::, etc.
type CommandProcessor struct {
	reporter     *Reporter
	stopToken    string // non-empty means commands are disabled until this token is seen
	debugEnabled bool
}

// NewCommandProcessor creates a processor wired to the given reporter.
// If debugEnabled is true, ::debug:: messages are included in the log output.
func NewCommandProcessor(rep *Reporter, debugEnabled bool) *CommandProcessor {
	return &CommandProcessor{
		reporter:     rep,
		debugEnabled: debugEnabled,
	}
}

// ProcessLine parses a log line for workflow commands.
// Returns nil if the line should be dropped from logs, or the line to emit.
func (p *CommandProcessor) ProcessLine(line string) *string {
	// If stop-commands is active, check for the resume token.
	if p.stopToken != "" {
		if line == "::"+p.stopToken+"::" {
			p.stopToken = ""
			return nil // drop the resume token line
		}
		return &line // pass through without command parsing
	}

	matches := cmdRegex.FindStringSubmatch(line)
	if matches == nil {
		return &line // not a command
	}

	command := strings.ToLower(matches[1])
	value := matches[3]

	switch command {
	case "add-mask":
		p.reporter.AddMask(value)
		return nil // drop from logs

	case "debug":
		if p.debugEnabled {
			return &line
		}
		return nil // drop debug lines when not enabled

	case "stop-commands":
		if value != "" {
			p.stopToken = value
		}
		return nil // drop the stop-commands line

	case "group", "endgroup", "error", "warning", "notice":
		return &line // pass through for frontend rendering

	default:
		return &line // unknown command, pass through
	}
}
