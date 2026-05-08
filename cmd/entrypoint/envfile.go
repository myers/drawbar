package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// parseEnvFile reads a GITHUB_ENV or GITHUB_OUTPUT style file.
// Format: key=value (one per line) or multiline with heredoc (key<<DELIM ... DELIM).
func parseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	result := make(map[string]string)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // 1MB max line

	for scanner.Scan() {
		line := scanner.Text()

		// Heredoc form is `KEY<<DELIMITER`; key=value form is `KEY=VALUE`.
		// They are mutually exclusive on a single line. A `<<` in the value
		// of a key=value line (e.g. `URL=foo<<bar`) must NOT be treated as
		// a heredoc start — only `<<` appearing in the KEY portion counts.
		eqIdx := strings.IndexByte(line, '=')
		hereIdx := strings.Index(line, "<<")
		if hereIdx > 0 && (eqIdx < 0 || hereIdx < eqIdx) {
			key := line[:hereIdx]
			delim := line[hereIdx+2:]
			var value strings.Builder
			found := false
			for scanner.Scan() {
				heredocLine := scanner.Text()
				if heredocLine == delim {
					found = true
					break
				}
				if value.Len() > 0 {
					value.WriteByte('\n')
				}
				value.WriteString(heredocLine)
			}
			if !found {
				return nil, fmt.Errorf("unclosed heredoc for key %q (delimiter %q not found)", key, delim)
			}
			result[key] = value.String()
			continue
		}

		// Simple key=value
		if eqIdx > 0 {
			key := line[:eqIdx]
			val := line[eqIdx+1:]
			result[key] = val
		}
	}

	return result, scanner.Err()
}

// parsePaths reads a GITHUB_PATH file (one path per line).
func parsePaths(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var paths []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			paths = append(paths, line)
		}
	}
	return paths, scanner.Err()
}
