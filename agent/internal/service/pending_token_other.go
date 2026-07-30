//go:build !windows

package service

func readPendingEnrollToken() string { return "" }

func clearPendingEnrollToken() {}
