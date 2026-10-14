package vm

import (
	"os"
	"strconv"
)

func osGetpid() int { return os.Getpid() }

func itoa(v int) string { return strconv.Itoa(v) }

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
