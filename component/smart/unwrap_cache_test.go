package smart

import (
	"testing"
	"time"
)

func TestUnwrapCacheTouchKeepsElectionAndCannotRenewReplacement(t *testing.T) {
	InitCache()
	store := &Store{}
	group, config, target := t.Name(), "test", "target"
	t.Cleanup(func() { store.DeleteUnwrapResult(group, config, target, "", nil) })
	now := time.Now()
	store.StoreUnwrapResult(group, config, target, "", UnwrapResult{Proxy: "NL", SelectedAt: now.Add(-time.Minute)})
	first, _ := store.GetUnwrapResult(group, config, target, "")
	store.TouchUnwrapResult(group, config, target, first)
	if result, expired := store.GetUnwrapResult(group, config, target, ""); result == nil || result != first || expired || !result.SelectedAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("touch changed the election: %v, expired=%t", result, expired)
	}

	store.StoreUnwrapResult(group, config, target, "", UnwrapResult{Proxy: "NL", SelectedAt: now})
	replacement, _ := store.GetUnwrapResult(group, config, target, "")
	key := FormatDBKey(config, group, target)
	shortExpiry := now.Add(time.Minute).Truncate(time.Second)
	unwrapCacheMu.Lock()
	unwrapCache.SetWithExpire(key, replacement, shortExpiry)
	unwrapCacheMu.Unlock()

	store.TouchUnwrapResult(group, config, target, first)
	store.DeleteUnwrapResult(group, config, target, "", first)
	unwrapCacheMu.Lock()
	result, expiry, _ := unwrapCache.GetWithExpire(key)
	unwrapCacheMu.Unlock()
	if result == first || result != replacement || !expiry.Equal(shortExpiry) {
		t.Fatalf("old election changed its replacement: %v expires=%s", result, expiry)
	}
}

func TestUnwrapCacheASNAliasRetainsElectionAndExpiry(t *testing.T) {
	InitCache()
	store := &Store{}
	group, config, asn := t.Name(), "test", "64512"
	t.Cleanup(func() {
		store.DeleteUnwrapResult(group, config, "first", asn, nil)
		store.DeleteUnwrapResult(group, config, "second", "", nil)
	})
	now := time.Now()
	store.StoreUnwrapResult(group, config, "first", asn, UnwrapResult{Proxy: "NL", SelectedAt: now.Add(-time.Minute)})
	first, _ := store.GetUnwrapResult(group, config, "first", asn)
	asnKey := FormatDBKey(config, group, asn)
	expiry := now.Add(time.Minute).Truncate(time.Second)
	unwrapCacheMu.Lock()
	unwrapCache.SetWithExpire(asnKey, first, expiry)
	unwrapCacheMu.Unlock()

	alias, expired := store.GetUnwrapResult(group, config, "second", asn)
	if alias != first || expired {
		t.Fatalf("ASN alias changed the election: %v, expired=%t", alias, expired)
	}
	unwrapCacheMu.Lock()
	_, copiedExpiry, _ := unwrapCache.GetWithExpire(FormatDBKey(config, group, "second"))
	unwrapCache.SetWithExpire(asnKey, first, now.Add(-time.Minute))
	unwrapCacheMu.Unlock()
	if !copiedExpiry.Equal(expiry) {
		t.Fatalf("ASN alias extended its expiry: %s, want %s", copiedExpiry, expiry)
	}
	if result, _ := store.GetUnwrapResult(group, config, "third", asn); result != nil {
		t.Fatalf("expired ASN alias returned a winner: %v", result)
	}
}
