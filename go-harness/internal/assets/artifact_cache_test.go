package assets

import (
	"crypto/sha256"
	"fmt"
	"testing"
)

func TestArtifactDigestCacheBoundAndIdentity(t *testing.T) {
	var cache artifactReadCache
	first := artifactReadKey{"first", "digest", 12}
	cache.put(first, [][sha256.Size]byte{{1}})
	for i := 0; i < artifactDigestCacheEntries-1; i++ {
		cache.put(artifactReadKey{fmt.Sprint(i), "digest", 12}, [][sha256.Size]byte{{2}})
	}
	if _, ok := cache.get(first); !ok {
		t.Fatal("live entry missing")
	}
	cache.put(artifactReadKey{"last", "digest", 12}, nil)
	if _, ok := cache.get(first); !ok {
		t.Fatal("recent entry evicted")
	}
	if _, ok := cache.get(artifactReadKey{"0", "digest", 12}); ok {
		t.Fatal("oldest retained")
	}
	if len(cache.entries) != artifactDigestCacheEntries {
		t.Fatal("cache exceeded bound")
	}
	for _, changed := range []artifactReadKey{{"first", "different", 12}, {"first", "digest", 13}, {"other", "digest", 12}} {
		if _, ok := cache.get(changed); ok {
			t.Fatal("unverified identity accepted")
		}
	}
}
