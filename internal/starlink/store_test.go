package starlink

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/awlx/starlink-manager/api/device"
	"google.golang.org/protobuf/proto"
)

func TestStoreLoadCorruptFileStartsEmpty(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir, 24*time.Hour, nil)
	bad := filepath.Join(dir, "store.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Load(); err != nil {
		t.Fatalf("Load on a corrupt file must not fail startup: %v", err)
	}
	if devices := store.Devices(); len(devices) != 0 {
		t.Fatalf("devices = %d, want 0 after corrupt load", len(devices))
	}
	// The corrupt file must be renamed aside, not deleted silently.
	entries, _ := os.ReadDir(dir)
	found := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "store.json.corrupt-") {
			found = true
		}
	}
	if !found {
		t.Fatal("corrupt store.json was not renamed aside")
	}
}

func TestStorePruneDevices(t *testing.T) {
	store := NewStore(t.TempDir(), 24*time.Hour, nil)
	store.UpsertDevice(&Device{ID: "ut1", Kind: KindDish, Name: "a", Active: true})
	store.UpsertDevice(&Device{ID: "ut2", Kind: KindDish, Name: "b", Active: true})
	store.UpsertDevice(&Device{ID: "r1", Kind: KindRouter, Name: "c", Active: true})

	removed := store.PruneDevices(map[string]struct{}{"ut1": {}, "r1": {}})
	if len(removed) != 1 || removed[0] != "ut2" {
		t.Fatalf("removed = %v, want [ut2]", removed)
	}
	ids := map[string]bool{}
	for _, d := range store.Devices() {
		ids[d.ID] = true
	}
	if ids["ut2"] {
		t.Fatal("pruned device still present")
	}
	if !ids["ut1"] || !ids["r1"] {
		t.Fatal("live devices must survive pruning")
	}
	// An empty live set must not wipe the registry (caller guards, but
	// the store itself only removes what is absent).
	removed = store.PruneDevices(map[string]struct{}{})
	if len(removed) != 2 {
		t.Fatalf("removed = %v, want ut1+r1", removed)
	}
}

func TestStoreSaveConcurrentReads(t *testing.T) {
	// Race smoke test: Save must not hold the store lock while marshaling
	// and writing, so page handlers stay responsive.
	store := NewStore(t.TempDir(), 24*time.Hour, nil)
	store.UpsertDevice(&Device{ID: "ut1", Kind: KindDish, Name: "a", Active: true})

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := store.Save(); err != nil {
				t.Errorf("Save: %v", err)
				return
			}
		}
	}()
	for i := 0; i < 200; i++ {
		_ = store.History("ut1", time.Hour)
		_ = store.Devices()
	}
	close(stop)
	wg.Wait()
}

func TestStoreSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir, 24*time.Hour, nil)
	store.UpsertDevice(&Device{ID: "ut1", Kind: KindDish, Name: "a", Active: true})
	now := time.Now().Unix()
	store.AppendHistory("ut1", &device.DishGetHistoryResponse{
		Current:          proto.Uint64(uint64(now)),
		PopPingLatencyMs: []float32{10, 11, 12},
		Obstructed:       []bool{true, false, false},
	})
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	// No tmp file may survive a successful save.
	if _, err := os.Stat(filepath.Join(dir, "store.json.tmp")); !os.IsNotExist(err) {
		t.Fatal("tmp file survived save")
	}
	store2 := NewStore(dir, 24*time.Hour, nil)
	if err := store2.Load(); err != nil {
		t.Fatal(err)
	}
	d, ok := store2.Device("ut1")
	if !ok {
		t.Fatal("device lost across save/load")
	}
	if len(store2.History("ut1", time.Hour)) == 0 {
		t.Fatal("history lost across save/load")
	}
	_ = d
}

func TestCookieJarMaxAge(t *testing.T) {
	jar := newCookieJar()
	u := &url.URL{Scheme: "https", Host: "api.example.test", Path: "/"}

	// Max-Age > 0 sets a rolling expiry.
	jar.SetCookies(u, []*http.Cookie{{Name: "a", Value: "v", MaxAge: 1, Path: "/"}})
	if got := jar.Cookies(u); len(got) != 1 || got[0].Value != "v" {
		t.Fatalf("cookies after Max-Age set = %v", got)
	}
	time.Sleep(1100 * time.Millisecond)
	if got := jar.Cookies(u); len(got) != 0 {
		t.Fatalf("expired Max-Age cookie still sent: %v", got)
	}

	// Max-Age <= 0 deletes the cookie even with an empty value (the
	// server-expired form), and must be handled before the empty-value
	// guard skips it.
	jar.SetCookies(u, []*http.Cookie{{Name: "b", Value: "v", Path: "/"}})
	jar.SetCookies(u, []*http.Cookie{{Name: "b", Value: "", MaxAge: -1, Path: "/"}})
	if got := jar.Cookies(u); len(got) != 0 {
		t.Fatalf("Max-Age delete ignored: %v", got)
	}
	jar.SetCookies(u, []*http.Cookie{{Name: "c", Value: "v", Path: "/"}})
	past := time.Now().Add(-time.Minute)
	jar.SetCookies(u, []*http.Cookie{{Name: "c", Value: "", Expires: past, Path: "/"}})
	if got := jar.Cookies(u); len(got) != 0 {
		t.Fatalf("expired-Expires empty-value delete ignored: %v", got)
	}
}
