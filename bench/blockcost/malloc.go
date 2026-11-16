package main

// #include <malloc.h>
import "C"

// mallocHeld is the account of glibc malloc for the C heap, over every arena.
// It returns what malloc holds from the system (its arenas and its mmapped
// chunks) and what of that is allocated. In a cgo build, the block caches and
// memtables of Pebble are calloc, and the Rust heap of the engine is malloc
// too. held minus used is free memory that malloc keeps, with fragmentation.
// If another allocator is preloaded, this reads the own heap of glibc, which
// is then empty.
func mallocHeld() (held, used uint64) {
	m := C.mallinfo2()
	return uint64(m.arena + m.hblkhd), uint64(m.uordblks + m.hblkhd)
}
