package main

import (
	"testing"
)

func TestWispQueryIndexStatements_AllHaveCreateIndex(t *testing.T) {
	if len(wispQueryIndexStatements) == 0 {
		t.Fatal("wispQueryIndexStatements must not be empty")
	}
	for _, stmt := range wispQueryIndexStatements {
		if len(stmt) < 20 {
			t.Errorf("statement too short to be a valid DDL: %q", stmt)
		}
	}
}
