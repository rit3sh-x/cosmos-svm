package ffi

// #include <malloc.h>
import "C"

// Malloc is glibc malloc's account of the C heap over every arena: held is
// what it holds from the system, its arenas and its mmapped chunks, and used
// is what of that is allocated. Pebble's block caches and memtables are calloc
// in a cgo build, and the engine's Rust heap is the system malloc too. held
// less used is free memory malloc keeps, fragmentation included. mallinfo2
// walks every free chunk under each arena's lock, so this is for a scrape, not
// for the block path. With another allocator preloaded both read glibc's own
// heap, which is then near empty.
func Malloc() (held, used uint64) {
	m := C.mallinfo2()
	return uint64(m.arena + m.hblkhd), uint64(m.uordblks + m.hblkhd)
}
