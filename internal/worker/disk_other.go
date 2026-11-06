//go:build !unix

package worker

func FreeDisk(string) int64 { return 0 }
