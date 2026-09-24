//go:build linux

package main

import "github.com/helmrdotdev/helmr/internal/nbd"

func runComputerHelper(args []string) (bool, error) {
	if len(args) != 3 || args[1] != "--nbd-helper" {
		return false, nil
	}
	return true, nbd.Helper(args[2])
}
