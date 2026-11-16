package main

import (
	"syscall"

	"github.com/syndtr/goleveldb/leveldb/filter"
	"github.com/syndtr/goleveldb/leveldb/opt"
)

// goleveldbOptions are the options svmd opened application.db with before it
// moved to Pebble. The harness keeps them, so a cell can still run on
// goleveldb beside Pebble, as every older result did.
func goleveldbOptions() *opt.Options {
	files := uint64(4096)
	var lim syscall.Rlimit
	if syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim) == nil {
		files = min(files, lim.Cur/2)
	}
	return &opt.Options{
		Filter:                 filter.NewBloomFilter(10),
		BlockCacheCapacity:     256 << 20,
		OpenFilesCacheCapacity: int(files),
		WriteBuffer:            32 << 20,
		CompactionTableSize:    8 << 20,
		DisableSeeksCompaction: true,
	}
}
