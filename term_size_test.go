package main

import "testing"

func TestWinsizeIoctlIsPlatformConstant(t *testing.T) {
	switch tiocgwinsz {
	case 0x5413, 0x40087468:
	default:
		t.Fatalf("TIOCGWINSZ = %#x, want linux 0x5413 or darwin 0x40087468", tiocgwinsz)
	}
}
