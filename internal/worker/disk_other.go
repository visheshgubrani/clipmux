//go:build !unix

package worker

func freeDisk(string) int64 { return 0 }
