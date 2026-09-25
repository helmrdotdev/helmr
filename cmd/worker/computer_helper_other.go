//go:build !linux

package main

func runComputerHelper([]string) (bool, error) { return false, nil }
