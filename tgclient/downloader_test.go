package tgclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"telecloud/config"

	"github.com/gotd/td/tg"
)

const testChunkSize = int64(1024 * 1024)

// newTestReader builds a reader backed by deterministic in-memory content instead
// of Telegram, and records how many fetches happen per offset. fetchDelay
// simulates network latency so that prefetches are still in flight when the
// synchronous path reaches them.
func newTestReader(t *testing.T, content []byte, depth int, fetchDelay time.Duration) (*tgFileReader, *fetchRecorder) {
	t.Helper()

	// Isolate from other tests: these are package-level singletons.
	globalChunkCache = NewBlockCache(128)
	globalPrefetchSem = make(chan struct{}, 32)

	prevPick := pickDownloadAPI
	pickDownloadAPI = func() *tg.Client { return testSessionAPI }
	t.Cleanup(func() { pickDownloadAPI = prevPick })

	msgID := int(nextTestMsgID.Add(1))
	rec := &fetchRecorder{perOffset: map[int64]int{}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	r := &tgFileReader{
		ctx:              ctx,
		cancel:           cancel,
		size:             int64(len(content)),
		msgID:            msgID,
		prefetchChunks:   make(map[int64][]byte),
		prefetchInflight: make(map[int64]chan struct{}),
		prefetchDepth:    depth,
		prefetchSem:      make(chan struct{}, depth),
		api:              testSessionAPI,
	}
	r.fetch = func(api *tg.Client, offset int64, limit int64) ([]byte, error) {
		rec.record(api, offset)
		if fetchDelay > 0 {
			time.Sleep(fetchDelay)
		}
		end := offset + limit
		if end > int64(len(content)) {
			end = int64(len(content))
		}
		if offset >= end {
			return nil, fmt.Errorf("offset %d out of range", offset)
		}
		return append([]byte(nil), content[offset:end]...), nil
	}

	// Registered last so it runs first: prefetch goroutines must be done before
	// the cleanups above restore the package-level hooks they read.
	t.Cleanup(r.waitForPrefetches)
	return r, rec
}

var nextTestMsgID atomic.Int64

// testSessionAPI is the reader's own session; fetches must always use it.
var testSessionAPI = &tg.Client{}

type fetchRecorder struct {
	mu        sync.Mutex
	perOffset map[int64]int
	apis      []*tg.Client
	total     int
}

func (f *fetchRecorder) record(api *tg.Client, offset int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.perOffset[offset]++
	f.apis = append(f.apis, api)
	f.total++
}

func (f *fetchRecorder) snapshot() (map[int64]int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[int64]int, len(f.perOffset))
	for k, v := range f.perOffset {
		out[k] = v
	}
	return out, f.total
}

func makeContent(n int64) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

// The whole point of streaming is serving correct bytes; prefetching must not
// corrupt or reorder them.
func TestReaderStreamsExactContent(t *testing.T) {
	content := makeContent(testChunkSize*5 + 12345)
	r, _ := newTestReader(t, content, 4, 0)

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("content mismatch: got %d bytes, want %d", len(got), len(content))
	}
}

// Reads after a seek must serve data from the new position, including seeks that
// land mid-chunk (the common case for HTTP Range requests).
func TestReaderSeekServesCorrectBytes(t *testing.T) {
	content := makeContent(testChunkSize*4 + 777)
	r, _ := newTestReader(t, content, 3, 0)

	for _, off := range []int64{0, 100, testChunkSize - 1, testChunkSize, testChunkSize*2 + 5000, int64(len(content)) - 10} {
		if _, err := r.Seek(off, io.SeekStart); err != nil {
			t.Fatalf("Seek(%d): %v", off, err)
		}
		buf := make([]byte, 4096)
		n, err := io.ReadFull(io.LimitReader(r, int64(len(buf))), buf)
		if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
			t.Fatalf("read at %d: %v", off, err)
		}
		want := content[off:min64(off+int64(n), int64(len(content)))]
		if !bytes.Equal(buf[:n], want) {
			t.Fatalf("mismatch after seek to %d", off)
		}
	}
}

// Regression guard for the read-ahead optimization: sequential streaming must
// actually issue concurrent fetches rather than one blocking fetch per chunk.
func TestReaderPrefetchesAheadOfCursor(t *testing.T) {
	content := makeContent(testChunkSize * 10)
	r, rec := newTestReader(t, content, 4, 0)

	// Read just the first byte, then drain the in-flight prefetches.
	buf := make([]byte, 1)
	if _, err := r.Read(buf); err != nil {
		t.Fatalf("Read: %v", err)
	}
	r.waitForPrefetches()

	offsets, _ := rec.snapshot()
	for i := int64(1); i <= 4; i++ {
		if offsets[i*testChunkSize] == 0 {
			t.Fatalf("chunk at offset %d was not prefetched; fetched offsets: %v", i*testChunkSize, offsets)
		}
	}
	if offsets[5*testChunkSize] != 0 {
		t.Fatalf("prefetched beyond the configured depth of 4")
	}
}

// A chunk already being prefetched must not be fetched a second time by the
// synchronous path, otherwise every chunk costs double the Telegram quota.
// The fetch delay makes the reader catch up with prefetches that are still in
// flight, which is exactly the case the in-flight dedup has to handle.
func TestReaderDoesNotFetchSameChunkTwice(t *testing.T) {
	content := makeContent(testChunkSize * 8)
	r, rec := newTestReader(t, content, 4, 20*time.Millisecond)

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("content mismatch")
	}
	r.waitForPrefetches()

	offsets, total := rec.snapshot()
	for off, count := range offsets {
		if count != 1 {
			t.Fatalf("chunk at offset %d fetched %d times, want exactly 1", off, count)
		}
	}
	if len(offsets) != 8 || total != 8 {
		t.Fatalf("fetched %d distinct chunks (%d total), want 8", len(offsets), total)
	}
}

// Seeking away must not leave results of superseded prefetches piling up in the
// per-reader buffer, which would grow memory without bound during heavy seeking.
func TestReaderDiscardsStalePrefetchAfterSeek(t *testing.T) {
	content := makeContent(testChunkSize * 20)
	r, _ := newTestReader(t, content, 4, 20*time.Millisecond)

	buf := make([]byte, 1)
	if _, err := r.Read(buf); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if _, err := r.Seek(15*testChunkSize, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	r.waitForPrefetches()

	r.prefetchMu.Lock()
	buffered := len(r.prefetchChunks)
	r.prefetchMu.Unlock()
	if buffered != 0 {
		t.Fatalf("prefetch buffer holds %d stale chunks after seek, want 0", buffered)
	}
}

// The server-wide read-ahead budget is best-effort: when it is exhausted (many
// concurrent viewers) prefetching must simply stop rather than block, leak the
// per-reader slot, or serve wrong bytes.
func TestReaderStreamsCorrectlyWhenGlobalBudgetExhausted(t *testing.T) {
	content := makeContent(testChunkSize*4 + 999)
	r, rec := newTestReader(t, content, 4, 0)

	// Take the entire global budget so no prefetch can start.
	for i := 0; i < cap(globalPrefetchSem); i++ {
		globalPrefetchSem <- struct{}{}
	}
	t.Cleanup(func() {
		for i := 0; i < cap(globalPrefetchSem); i++ {
			<-globalPrefetchSem
		}
	})

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("content mismatch with prefetch disabled")
	}

	// Per-reader slots must all have been returned, not leaked by the rejected path.
	if len(r.prefetchSem) != 0 {
		t.Fatalf("leaked %d per-reader prefetch slots", len(r.prefetchSem))
	}
	_, total := rec.snapshot()
	if total != 5 {
		t.Fatalf("fetched %d chunks, want 5 (synchronous only, no prefetch)", total)
	}
}

// Closing the reader mid-stream (client aborts the request, very common with
// video players) must not leave Read blocked forever waiting on a prefetch.
func TestReaderReadUnblocksOnClose(t *testing.T) {
	content := makeContent(testChunkSize * 6)
	r, _ := newTestReader(t, content, 4, 50*time.Millisecond)

	buf := make([]byte, 1)
	if _, err := r.Read(buf); err != nil {
		t.Fatalf("Read: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		// Jump to a chunk that a prefetch goroutine is still working on.
		if _, err := r.Seek(2*testChunkSize, io.SeekStart); err != nil {
			done <- err
			return
		}
		big := make([]byte, 4096)
		_, err := r.Read(big)
		done <- err
	}()

	r.Close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Read did not return after Close")
	}
}

// waitForPrefetches blocks until every in-flight prefetch goroutine has finished.
func (r *tgFileReader) waitForPrefetches() {
	for i := 0; i < cap(r.prefetchSem); i++ {
		r.prefetchSem <- struct{}{}
	}
	for i := 0; i < cap(r.prefetchSem); i++ {
		<-r.prefetchSem
	}
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// stubSessions wires fetchChunk seams: testSessionAPI is the reader's session,
// altAPI another one whose file location differs.
func stubSessions(t *testing.T, r *tgFileReader, altAPI *tg.Client, altLoc tg.InputFileLocationClass, getFile func(api *tg.Client, req *tg.UploadGetFileRequest) (tg.UploadFileClass, error)) {
	t.Helper()
	prevResolve, prevGet, prevAlt, prevMark := resolveLocation, uploadGetFile, pickAltAPI, markBotCooldown
	t.Cleanup(func() {
		resolveLocation, uploadGetFile, pickAltAPI, markBotCooldown = prevResolve, prevGet, prevAlt, prevMark
	})
	resolveLocation = func(_ context.Context, api *tg.Client, _ int, _ *config.Config) (tg.InputFileLocationClass, error) {
		if api == altAPI {
			return altLoc, nil
		}
		return nil, errors.New("unexpected resolve")
	}
	uploadGetFile = func(_ context.Context, api *tg.Client, req *tg.UploadGetFileRequest) (tg.UploadFileClass, error) {
		return getFile(api, req)
	}
	pickAltAPI = func() *tg.Client { return altAPI }
	markBotCooldown = func(*tg.Client, int) {}
}

func TestFetchChunkFloodWaitSwitchUsesOwnLocation(t *testing.T) {
	r, _ := newTestReader(t, makeContent(testChunkSize), 1, 0)
	origLoc := &tg.InputDocumentFileLocation{ID: 1}
	altLoc := &tg.InputDocumentFileLocation{ID: 2}
	r.locs = map[*tg.Client]tg.InputFileLocationClass{testSessionAPI: origLoc}
	altAPI := &tg.Client{}
	stubSessions(t, r, altAPI, altLoc, func(api *tg.Client, req *tg.UploadGetFileRequest) (tg.UploadFileClass, error) {
		if api == testSessionAPI {
			return nil, errors.New("FLOOD_WAIT_1")
		}
		if req.Location != altLoc {
			return nil, errors.New("FILE_REFERENCE_EXPIRED")
		}
		return &tg.UploadFile{Bytes: make([]byte, req.Limit)}, nil
	})

	data, err := r.fetchChunk(testSessionAPI, 0, testChunkSize)
	if err != nil || int64(len(data)) != testChunkSize {
		t.Fatalf("fetchChunk = %d bytes, err %v", len(data), err)
	}
	if r.locs[testSessionAPI] != origLoc {
		t.Fatal("r.loc was changed by a retry on another session")
	}
}

// multiSessionFixture wires a reader whose sessions each have their own file
// location, using the real fetchChunk against a stubbed Telegram.
type multiSessionFixture struct {
	r        *tgFileReader
	sessions []*tg.Client // sessions[0] is the reader's own
	locOf    map[*tg.Client]*tg.InputDocumentFileLocation
	mu       sync.Mutex
	resolves map[*tg.Client]int
	bad      map[*tg.Client]bool // sessions that cannot resolve the file
	wrongLoc atomic.Int32        // fetches that carried another session's location
	used     map[*tg.Client]int
}

func newMultiSessionFixture(t *testing.T, content []byte, n int) *multiSessionFixture {
	r, _ := newTestReader(t, content, 4, 0)
	f := &multiSessionFixture{
		r:        r,
		locOf:    map[*tg.Client]*tg.InputDocumentFileLocation{},
		resolves: map[*tg.Client]int{},
		bad:      map[*tg.Client]bool{},
		used:     map[*tg.Client]int{},
	}
	f.sessions = append(f.sessions, testSessionAPI)
	for i := 1; i < n; i++ {
		f.sessions = append(f.sessions, &tg.Client{})
	}
	for i, s := range f.sessions {
		f.locOf[s] = &tg.InputDocumentFileLocation{ID: int64(i + 1)}
	}
	r.locs = map[*tg.Client]tg.InputFileLocationClass{testSessionAPI: f.locOf[testSessionAPI]}
	r.fetch = r.fetchChunk

	prevResolve, prevGet, prevPick := resolveLocation, uploadGetFile, pickDownloadAPI
	t.Cleanup(func() { resolveLocation, uploadGetFile, pickDownloadAPI = prevResolve, prevGet, prevPick })
	resolveLocation = func(_ context.Context, api *tg.Client, _ int, _ *config.Config) (tg.InputFileLocationClass, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.resolves[api]++
		if f.bad[api] {
			return nil, errors.New("message not found")
		}
		return f.locOf[api], nil
	}
	uploadGetFile = func(_ context.Context, api *tg.Client, req *tg.UploadGetFileRequest) (tg.UploadFileClass, error) {
		f.mu.Lock()
		f.used[api]++
		f.mu.Unlock()
		if req.Location != tg.InputFileLocationClass(f.locOf[api]) {
			f.wrongLoc.Add(1)
			return nil, errors.New("FILE_REFERENCE_EXPIRED")
		}
		end := req.Offset + int64(req.Limit)
		if end > int64(len(content)) {
			end = int64(len(content))
		}
		return &tg.UploadFile{Bytes: append([]byte(nil), content[req.Offset:end]...)}, nil
	}
	var next atomic.Int32
	pickDownloadAPI = func() *tg.Client {
		return f.sessions[int(next.Add(1))%len(f.sessions)]
	}
	return f
}

// Prefetches rotate over several sessions; each fetch must carry the location
// resolved for its own session, and a session is resolved at most once.
func TestPrefetchUsesPerSessionLocations(t *testing.T) {
	content := makeContent(10 * testChunkSize)
	f := newMultiSessionFixture(t, content, 3)

	got, err := io.ReadAll(f.r)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("content mismatch, err %v", err)
	}
	f.r.waitForPrefetches()

	if n := f.wrongLoc.Load(); n != 0 {
		t.Fatalf("%d fetches carried another session's location", n)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.sessions[1:] {
		if f.used[s] == 0 {
			t.Error("a session was never used for prefetching")
		}
		if f.resolves[s] != 1 {
			t.Errorf("resolve calls = %d, want 1 per session", f.resolves[s])
		}
	}
	if f.resolves[testSessionAPI] != 0 {
		t.Error("reader's own session was re-resolved")
	}
}

func TestFileRefExpiredReResolvesOnlyThatSession(t *testing.T) {
	f := newMultiSessionFixture(t, makeContent(testChunkSize), 2)
	a, b := f.sessions[0], f.sessions[1]
	freshB := f.locOf[b]
	staleB := &tg.InputDocumentFileLocation{ID: 99}
	f.r.locs[b] = staleB
	locA := f.r.locs[a]

	data, err := f.r.fetchChunk(b, 0, testChunkSize)
	if err != nil || int64(len(data)) != testChunkSize {
		t.Fatalf("fetchChunk = %d bytes, err %v", len(data), err)
	}
	if f.r.locs[b] != tg.InputFileLocationClass(freshB) {
		t.Error("B's location was not refreshed")
	}
	if f.r.locs[a] != locA {
		t.Error("A's location changed")
	}
	if f.resolves[a] != 0 || f.resolves[b] != 1 {
		t.Errorf("resolves A=%d B=%d, want 0 and 1", f.resolves[a], f.resolves[b])
	}
}

func TestPrefetchFallsBackWhenSessionCannotResolve(t *testing.T) {
	content := makeContent(8 * testChunkSize)
	f := newMultiSessionFixture(t, content, 3)
	f.bad[f.sessions[1]] = true

	got, err := io.ReadAll(f.r)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("content mismatch, err %v", err)
	}
	f.r.waitForPrefetches()
	if f.used[f.sessions[1]] != 0 {
		t.Error("unresolvable session was used for a fetch")
	}
	if f.wrongLoc.Load() != 0 {
		t.Error("a fetch carried another session's location")
	}
}

// A session that cannot resolve the file is asked once per reader, and other
// readers of the same message skip it for the negative-cache window.
func TestFailedSessionResolvedOnce(t *testing.T) {
	content := makeContent(12 * testChunkSize)
	f := newMultiSessionFixture(t, content, 3)
	bad := f.sessions[1]
	f.bad[bad] = true

	got, err := io.ReadAll(f.r)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("content mismatch, err %v", err)
	}
	f.r.waitForPrefetches()
	f.mu.Lock()
	n := f.resolves[bad]
	f.mu.Unlock()
	if n != 1 {
		t.Fatalf("bad session resolved %d times by one reader, want 1", n)
	}

	r2, _ := newTestReader(t, content, 4, 0)
	r2.msgID = f.r.msgID
	r2.locs = map[*tg.Client]tg.InputFileLocationClass{testSessionAPI: f.locOf[testSessionAPI]}
	r2.fetch = r2.fetchChunk
	if got, err := io.ReadAll(r2); err != nil || !bytes.Equal(got, content) {
		t.Fatalf("second reader content mismatch, err %v", err)
	}
	r2.waitForPrefetches()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resolves[bad] != 1 {
		t.Fatalf("bad session resolved %d times in total, want 1", f.resolves[bad])
	}
}

// Reusing a cached location must not extend its lifetime.
func TestLocForCacheHitKeepsExpiry(t *testing.T) {
	f := newMultiSessionFixture(t, makeContent(testChunkSize), 2)
	b := f.sessions[1]
	expires := time.Now().Add(5 * time.Minute)
	key := sessionLocKey{f.r.msgID, b}
	cacheMutex.Lock()
	sessionLocCache[key] = &cachedLocation{loc: f.locOf[b], api: b, expiresAt: expires}
	cacheMutex.Unlock()

	loc, err := f.r.locFor(b)
	if err != nil || loc != tg.InputFileLocationClass(f.locOf[b]) {
		t.Fatalf("locFor = %v, %v", loc, err)
	}
	cacheMutex.RLock()
	got := sessionLocCache[key].expiresAt
	cacheMutex.RUnlock()
	if !got.Equal(expires) {
		t.Errorf("cache expiry changed from %v to %v", expires, got)
	}
	if f.resolves[b] != 0 {
		t.Errorf("resolved %d times despite cache hit", f.resolves[b])
	}
}
