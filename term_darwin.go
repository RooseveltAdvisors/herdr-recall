//go:build darwin

package main

// TIOCGWINSZ is _IOR('t', 104, struct winsize) on Darwin.
const tiocgwinsz = 0x40087468
