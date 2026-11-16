package svm

import (
	"testing"
	"time"

	"github.com/rit3sh-x/cosmos-svm/ffi"
)

// TestAFullFeedTakesALowerHeight proves that Put of a height below every
// buffered one into a full buffer returns and evicts the oldest height. An
// eviction search that starts at the new height finds nothing older, deletes
// nothing and loops forever under the lock.
func TestAFullFeedTakesALowerHeight(t *testing.T) {
	f := NewFeed()
	writes := []AccountValue{{}}
	for h := uint64(100); h < 100+maxFeedHeights; h++ {
		f.Put(h, writes, nil)
	}
	done := make(chan struct{})
	go func() {
		f.Put(1, writes, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Put of a lower height into a full buffer did not return")
	}
	if len(f.buf) != maxFeedHeights {
		t.Fatalf("the buffer holds %d heights, want %d", len(f.buf), maxFeedHeights)
	}
	if _, kept := f.buf[1]; !kept {
		t.Fatal("want height 1 kept")
	}
	if _, kept := f.buf[100]; kept {
		t.Fatal("want height 100, the oldest before height 1, evicted")
	}
}

// TestTheFeedWatchesKeysAndOwners checks what the feed watches. An account
// subscription watches its key. A program subscription watches the owner in a
// written value. When two subscriptions hold a key, the key stays watched
// until both release it.
func TestTheFeedWatchesKeysAndOwners(t *testing.T) {
	f := NewFeed()
	account, program, other := [32]byte{1}, [32]byte{2}, [32]byte{3}
	owned := func(owner [32]byte) []byte { return ffi.EncodeAccount(&owner, 1, 0, false, nil) }
	f.Hold(account, false)
	f.Hold(account, false)
	f.Hold(program, true)
	for i, tt := range []struct {
		pubkey [32]byte
		value  []byte
		want   bool
	}{
		{account, owned(other), true},
		{other, owned(program), true},
		{other, owned(other), false},
		{program, owned(other), false},
		{other, nil, false},
	} {
		if got := f.Watches(tt.pubkey, tt.value); got != tt.want {
			t.Fatalf("case %d: Watches = %v, want %v", i, got, tt.want)
		}
	}
	f.Release(account, false)
	if !f.Watches(account, owned(other)) {
		t.Fatal("a key one subscription still holds is not watched")
	}
	f.Release(account, false)
	f.Release(program, true)
	if f.Wants() || f.Watches(account, owned(other)) || f.Watches(other, owned(program)) {
		t.Fatal("released keys are still watched")
	}
}
