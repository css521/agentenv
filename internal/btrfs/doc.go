// Package btrfs wraps btrfs subvolume/snapshot operations (cgo). Its real
// implementation is built only with the "btrfs" build tag; this file keeps the
// package non-empty for default (CGO-free) builds so `go build ./...` succeeds
// without libbtrfs.
package btrfs
