package main

/*
#include <malloc.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

// in_env reports whether the operator set a malloc variable, by its own name
// or through GLIBC_TUNABLES. glibc has then applied it already.
static int in_env(const char *name, const char *tunable) {
	const char *t = getenv("GLIBC_TUNABLES");
	return getenv(name) != NULL || (t != NULL && strstr(t, tunable) != NULL);
}

// tune_malloc caps glibc at four arenas and trims an arena's top at 128 KiB,
// unless the environment sets either. It runs among the ELF constructors,
// before Go's runtime starts, while the process has one thread and one arena.
// A cap set later would not apply to the arenas made before it, and after
// glibc made nine arenas a cap has no effect. The cgo runtime calls malloc on
// each thread that it starts.
__attribute__((constructor)) static void tune_malloc(void) {
	if (!in_env("MALLOC_ARENA_MAX", "glibc.malloc.arena_max"))
		mallopt(M_ARENA_MAX, 4);
	if (!in_env("MALLOC_TRIM_THRESHOLD_", "glibc.malloc.trim_threshold"))
		mallopt(M_TRIM_THRESHOLD, 131072);
}

// The probes below are for TestMallocStartsTuned. volatile keeps the compiler
// from removing a malloc whose block is only freed.

static pthread_barrier_t probe_barrier;

static void *probe_thread(void *arg) {
	void *volatile p = malloc(64);
	pthread_barrier_wait(&probe_barrier);
	free(p);
	return arg;
}

// probe_arenas has sixteen threads each hold a block at once, so glibc makes
// arenas up to its limit, and counts the arenas malloc_info lists.
static int probe_arenas(void) {
	enum { threads = 16 };
	pthread_t t[threads];
	pthread_barrier_init(&probe_barrier, NULL, threads);
	for (int i = 0; i < threads; i++)
		pthread_create(&t[i], NULL, probe_thread, NULL);
	for (int i = 0; i < threads; i++)
		pthread_join(t[i], NULL);
	pthread_barrier_destroy(&probe_barrier);
	char *xml;
	size_t len;
	FILE *f = open_memstream(&xml, &len);
	malloc_info(0, f);
	fclose(f);
	int arenas = 0;
	for (char *s = xml; (s = strstr(s, "<heap nr=")) != NULL; s++)
		arenas++;
	free(xml);
	return arenas;
}

// probe_trims frees an 8 MiB block, which raises glibc's trim threshold to
// 16 MiB unless a threshold was set, then frees 4 MiB of 64 KiB blocks and
// reports whether the arena gave most of it back to the system.
static int probe_trims(void) {
	enum { blocks = 64, size = 64 << 10 };
	void *volatile big = malloc(8 << 20);
	free(big);
	void *volatile p[blocks];
	for (int i = 0; i < blocks; i++)
		p[i] = malloc(size);
	size_t held = mallinfo2().arena;
	for (int i = blocks - 1; i >= 0; i--)
		free(p[i]);
	return mallinfo2().arena + blocks * size / 2 < held;
}
*/
import "C"

// mallocArenas and mallocTrims run the probes for TestMallocStartsTuned,
// which cannot use cgo itself.
func mallocArenas() int { return int(C.probe_arenas()) }
func mallocTrims() bool { return C.probe_trims() != 0 }
