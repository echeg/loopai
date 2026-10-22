package runs

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// statStartTimeField is the 1-based position of starttime in /proc/<pid>/stat.
const statStartTimeField = 22

// processStartToken returns field 22 of /proc/<pid>/stat, the process start time in clock ticks
// since boot.
func processStartToken(pid int) (string, error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", fmt.Errorf("process start token of pid %d: %w", pid, err)
	}
	token, err := parseStatStartTime(string(data))
	if err != nil {
		return "", fmt.Errorf("process start token of pid %d: %w", pid, err)
	}
	return token, nil
}

// parseStatStartTime extracts starttime from the contents of /proc/<pid>/stat. The second
// field is the command name in parentheses and may itself contain spaces and parentheses, so
// fields are counted from the last ')'; the field after it is field 3.
func parseStatStartTime(stat string) (string, error) {
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return "", errors.New("malformed stat: no command name")
	}
	fields := strings.Fields(stat[end+1:])
	idx := statStartTimeField - 3
	if len(fields) <= idx {
		return "", fmt.Errorf("malformed stat: %d fields after the command name", len(fields))
	}
	return fields[idx], nil
}
