//go:build !unix

package server

func diskSpace(string) (free, total float64) { return 0, 0 }
