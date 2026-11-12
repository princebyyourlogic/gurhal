// SPDX-License-Identifier: GPL-3.0-or-later

// Package auth signs players in and hands the login key from the login service to the
// game service.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"sync"
	"time"
)

// keyTTL is how long a login key waits for the game connection.
const keyTTL = 2 * time.Minute

type issued struct {
	key     [4]byte
	disc    string
	expires time.Time
}

// Keys holds the 4-byte key login sends in 0x0223 and the game server takes once on
// 0x020D, with the disc the player logged in from.
type Keys struct {
	mu   sync.Mutex
	keys map[uint32]issued
	now  func() time.Time
}

func NewKeys() *Keys { return &Keys{keys: map[uint32]issued{}, now: time.Now} }

// Issue makes a fresh key for gid, replacing any earlier one.
func (k *Keys) Issue(gid uint32, disc string) [4]byte {
	var key [4]byte
	rand.Read(key[:])
	k.mu.Lock()
	defer k.mu.Unlock()
	now := k.now()
	for g, v := range k.keys {
		if now.After(v.expires) {
			delete(k.keys, g)
		}
	}
	k.keys[gid] = issued{key: key, disc: disc, expires: now.Add(keyTTL)}
	return key
}

// Take returns the disc for gid's key, once. A wrong key also uses it up.
func (k *Keys) Take(gid uint32, key []byte) (string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.keys[gid]
	delete(k.keys, gid)
	if !ok || k.now().After(v.expires) || subtle.ConstantTimeCompare(v.key[:], key) != 1 {
		return "", false
	}
	return v.disc, true
}
