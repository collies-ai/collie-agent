//go:build !darwin && !linux

package cli

// stdinIsTerminal is false where collie-agent cannot tell a terminal apart: pair asks nothing there.
func stdinIsTerminal() bool { return false }
