// SPDX-License-Identifier: Apache-2.0

package core

import "sync"

// keyedMutex hands out a per-key mutex, like sync.Map would, but forgets a key
// once its last holder unlocks. Without this, a long-lived Reconciler whose
// project/branch keys churn (branches come and go) grows its lock set forever,
// even after every environment for a key has been destroyed.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*refCountedMutex
}

// refCountedMutex is one keyed entry: the mutex callers actually lock, plus a
// count of how many callers currently hold or are waiting on it, so the entry
// can be safely deleted only once nobody references it anymore.
type refCountedMutex struct {
	mu   sync.Mutex
	refs int
}

func newKeyedMutex() *keyedMutex {
	return &keyedMutex{locks: make(map[string]*refCountedMutex)}
}

// Lock acquires the mutex for key, creating it if necessary, and returns a
// func that releases it. The entry is removed from the map once release has
// run and no other caller is holding or waiting on it.
func (k *keyedMutex) Lock(key string) func() {
	k.mu.Lock()
	e, ok := k.locks[key]
	if !ok {
		e = &refCountedMutex{}
		k.locks[key] = e
	}
	e.refs++
	k.mu.Unlock()

	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		k.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}

// len reports the number of distinct keys currently tracked. It exists for
// tests verifying idle entries are actually reclaimed.
func (k *keyedMutex) len() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.locks)
}
