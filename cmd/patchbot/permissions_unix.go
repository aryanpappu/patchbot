//go:build unix

package main

import "syscall"

// Set once, before starting any work. Git and repository subprocesses inherit
// this mask too. Explicit chmod by trusted repository code is outside our jail.
func privateUmask() { syscall.Umask(0o077) }
