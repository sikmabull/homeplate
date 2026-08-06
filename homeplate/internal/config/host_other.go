//go:build !darwin && !linux

package config

// HostMemoryBytes is a conservative fallback on unsupported platforms.
func HostMemoryBytes() int64 { return 8 << 30 }
