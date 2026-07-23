package main

import (
	"os"
	"strings"
)

func gcDoltSkip() bool {
	v := strings.TrimSpace(os.Getenv("GC_BEADS_SKIP"))
	return v == "1" || v == "true" || v == "skip"
}
