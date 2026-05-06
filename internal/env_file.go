package internal

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// LoadEnvFile loads KEY=VALUE pairs from an explicit env file into the process
// environment. It intentionally does not auto-discover .env files.
func LoadEnvFile(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}

	expandedPath, err := expandEnvFilePath(path)
	if err != nil {
		return err
	}

	file, err := os.Open(expandedPath)
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))

		key, rawValue, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("%s:%d: expected KEY=VALUE", expandedPath, lineNo)
		}
		key = strings.TrimSpace(key)
		if !validEnvFileKey(key) {
			return fmt.Errorf("%s:%d: invalid environment variable name %q", expandedPath, lineNo, key)
		}

		value, err := parseEnvFileValue(strings.TrimSpace(rawValue))
		if err != nil {
			return fmt.Errorf("%s:%d: %w", expandedPath, lineNo, err)
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("%s:%d: setting %s: %w", expandedPath, lineNo, key, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}

func expandEnvFilePath(path string) (string, error) {
	path = os.ExpandEnv(path)
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if path == "~" {
			path = home
		} else {
			path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return filepath.Clean(path), nil
}

func parseEnvFileValue(value string) (string, error) {
	if value == "" {
		return "", nil
	}

	if strings.HasPrefix(value, "'") {
		if !strings.HasSuffix(value, "'") || len(value) == 1 {
			return "", fmt.Errorf("unterminated single-quoted value")
		}
		return strings.TrimSuffix(strings.TrimPrefix(value, "'"), "'"), nil
	}

	if strings.HasPrefix(value, "\"") {
		if !strings.HasSuffix(value, "\"") || len(value) == 1 {
			return "", fmt.Errorf("unterminated double-quoted value")
		}
		unquoted, err := strconv.Unquote(value)
		if err != nil {
			return "", fmt.Errorf("invalid double-quoted value: %w", err)
		}
		return unquoted, nil
	}

	return value, nil
}

func validEnvFileKey(key string) bool {
	if key == "" {
		return false
	}
	for i := 0; i < len(key); i++ {
		b := key[i]
		if i == 0 {
			if b != '_' && (b < 'A' || b > 'Z') && (b < 'a' || b > 'z') {
				return false
			}
			continue
		}
		if b != '_' && (b < 'A' || b > 'Z') && (b < 'a' || b > 'z') && (b < '0' || b > '9') {
			return false
		}
	}
	return true
}
