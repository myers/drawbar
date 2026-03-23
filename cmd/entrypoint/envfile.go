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

		// Check for heredoc: key<<DELIMITER
		if idx := strings.Index(line, "<<"); idx > 0 {
			key := line[:idx]
			delim := line[idx+2:]
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
		if idx := strings.IndexByte(line, '='); idx > 0 {
			key := line[:idx]
			val := line[idx+1:]
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
