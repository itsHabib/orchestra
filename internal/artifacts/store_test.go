package artifacts

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFileStorePutGetRoundTrip(t *testing.T) {
	store := newTestStore(t)
	cases := putGetCases()
	for i := range cases {
		tc := &cases[i]
		t.Run(tc.name, func(t *testing.T) {
			runPutGetCase(t, store, tc)
		})
	}
}

type putGetCase struct {
	name    string
	agent   string
	key     string
	phase   string
	art     Artifact
	expSize int64
}

func putGetCases() []putGetCase {
	return []putGetCase{
		{
			name:    "text",
			agent:   "designer",
			key:     "design_doc",
			phase:   "design",
			art:     Artifact{Type: TypeText, Content: json.RawMessage(`"hello world"`)},
			expSize: int64(len(`"hello world"`)),
		},
		{
			name:    "json object",
			agent:   "critic",
			key:     "verdict",
			phase:   "design",
			art:     Artifact{Type: TypeJSON, Content: json.RawMessage(`{"decision":"proceed"}`)},
			expSize: int64(len(`{"decision":"proceed"}`)),
		},
		{
			name:    "json array",
			agent:   "lister",
			key:     "items",
			art:     Artifact{Type: TypeJSON, Content: json.RawMessage(`[1,2,3]`)},
			expSize: int64(len(`[1,2,3]`)),
		},
	}
}

func runPutGetCase(t *testing.T, store *FileStore, tc *putGetCase) {
	t.Helper()
	ctx := context.Background()
	meta, err := store.Put(ctx, "run-1", tc.agent, tc.key, tc.phase, tc.art)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	assertPutMeta(t, &meta, tc)

	gotArt, gotMeta, err := store.Get(ctx, "run-1", tc.agent, tc.key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !reflect.DeepEqual(gotArt, tc.art) {
		t.Errorf("Get artifact = %+v, want %+v", gotArt, tc.art)
	}
	if gotMeta.Size != meta.Size || gotMeta.Type != meta.Type || gotMeta.Phase != meta.Phase {
		t.Errorf("Get meta = %+v, want size/type/phase to match Put meta %+v", gotMeta, meta)
	}
}

func assertPutMeta(t *testing.T, meta *Meta, tc *putGetCase) {
	t.Helper()
	if meta.Size != tc.expSize {
		t.Errorf("Size = %d, want %d", meta.Size, tc.expSize)
	}
	if meta.Type != tc.art.Type {
		t.Errorf("Type = %q, want %q", meta.Type, tc.art.Type)
	}
	if meta.Phase != tc.phase {
		t.Errorf("Phase = %q, want %q", meta.Phase, tc.phase)
	}
	if meta.Key != tc.key {
		t.Errorf("Key = %q, want %q", meta.Key, tc.key)
	}
	if meta.Written.IsZero() {
		t.Error("Written should be set")
	}
}

func TestFileStoreGetNotFound(t *testing.T) {
	store := newTestStore(t)
	_, _, err := store.Get(context.Background(), "run-1", "designer", "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get: err = %v, want ErrNotFound", err)
	}
}

func TestFileStorePutAlreadyExists(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	art := Artifact{Type: TypeText, Content: json.RawMessage(`"first"`)}
	if _, err := store.Put(ctx, "r", "a", "k", "", art); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	_, err := store.Put(ctx, "r", "a", "k", "", art)
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("second Put: err = %v, want ErrAlreadyExists", err)
	}
}

func TestFileStorePutValidation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	cases := []struct {
		name    string
		agent   string
		key     string
		art     Artifact
		wantErr string
	}{
		{name: "empty agent", agent: "", key: "k", art: Artifact{Type: TypeText, Content: json.RawMessage(`"x"`)}, wantErr: "agent is empty"},
		{name: "empty key", agent: "a", key: "", art: Artifact{Type: TypeText, Content: json.RawMessage(`"x"`)}, wantErr: "key is empty"},
		{name: "agent traversal", agent: "../etc", key: "k", art: Artifact{Type: TypeText, Content: json.RawMessage(`"x"`)}, wantErr: "invalid characters"},
		{name: "key traversal", agent: "a", key: "../passwd", art: Artifact{Type: TypeText, Content: json.RawMessage(`"x"`)}, wantErr: "invalid characters"},
		{name: "key dot", agent: "a", key: ".", art: Artifact{Type: TypeText, Content: json.RawMessage(`"x"`)}, wantErr: "reserved"},
		{name: "leading dot", agent: "a", key: ".hidden", art: Artifact{Type: TypeText, Content: json.RawMessage(`"x"`)}, wantErr: "must not start with a dot"},
		{name: "bad type", agent: "a", key: "k", art: Artifact{Type: Type("xml"), Content: json.RawMessage(`"x"`)}, wantErr: `type "xml"`},
		{name: "empty content", agent: "a", key: "k", art: Artifact{Type: TypeText, Content: nil}, wantErr: "content is empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := store.Put(ctx, "r", tc.agent, tc.key, "", tc.art)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %q, want contains %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestFileStoreList(t *testing.T) {
	store := newTestStore(t)
	seedListFixtures(t, store)
	t.Run("agent filter", func(t *testing.T) { assertListAgentFilter(t, store) })
	t.Run("all agents", func(t *testing.T) { assertListAllAgents(t, store) })
}

func seedListFixtures(t *testing.T, store *FileStore) {
	t.Helper()
	ctx := context.Background()
	seed := []struct {
		agent, key, phase string
		art               Artifact
	}{
		{"alpha", "a1", "design", Artifact{Type: TypeText, Content: json.RawMessage(`"a1-content"`)}},
		{"alpha", "a2", "review", Artifact{Type: TypeJSON, Content: json.RawMessage(`{"k":"v"}`)}},
		{"beta", "b1", "design", Artifact{Type: TypeText, Content: json.RawMessage(`"b1-content"`)}},
	}
	for _, s := range seed {
		if _, err := store.Put(ctx, "r", s.agent, s.key, s.phase, s.art); err != nil {
			t.Fatalf("seed Put %s/%s: %v", s.agent, s.key, err)
		}
	}
}

func assertListAgentFilter(t *testing.T, store *FileStore) {
	t.Helper()
	got, err := store.List(context.Background(), "r", "alpha")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Key != "a1" || got[1].Key != "a2" {
		t.Errorf("keys = [%s, %s], want [a1, a2]", got[0].Key, got[1].Key)
	}
	if got[0].Agent != "alpha" {
		t.Errorf("Agent = %q, want alpha", got[0].Agent)
	}
	if got[0].Phase != "design" || got[1].Phase != "review" {
		t.Errorf("phases = [%s, %s], want [design, review]", got[0].Phase, got[1].Phase)
	}
}

func assertListAllAgents(t *testing.T, store *FileStore) {
	t.Helper()
	got, err := store.List(context.Background(), "r", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	want := []struct{ agent, key string }{
		{"alpha", "a1"}, {"alpha", "a2"}, {"beta", "b1"},
	}
	for i, w := range want {
		if got[i].Agent != w.agent || got[i].Key != w.key {
			t.Errorf("got[%d] = %s/%s, want %s/%s", i, got[i].Agent, got[i].Key, w.agent, w.key)
		}
	}
}

func TestFileStoreListMissingRoot(t *testing.T) {
	// Root that does not exist. List should return empty, not error.
	store := NewFileStore(filepath.Join(t.TempDir(), "no-such-dir"))
	got, err := store.List(context.Background(), "r", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got != nil {
		t.Errorf("want nil, got %v", got)
	}
	gotAgent, err := store.List(context.Background(), "r", "a")
	if err != nil {
		t.Fatalf("List(agent): %v", err)
	}
	if gotAgent != nil {
		t.Errorf("want nil, got %v", gotAgent)
	}
}

func TestFileStoreNoTempLeftover(t *testing.T) {
	// AtomicWrite creates a .tmp; on success it should be renamed away.
	store := newTestStore(t)
	if _, err := store.Put(context.Background(), "r", "a", "k", "", Artifact{
		Type:    TypeText,
		Content: json.RawMessage(`"x"`),
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	tmp := filepath.Join(store.Root(), "a", "k.json.tmp")
	if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("temp file %s should not exist after successful Put: stat err = %v", tmp, err)
	}
}

// newTestStore returns a FileStore rooted in t.TempDir() with a fixed clock so
// envelope output is byte-stable. The clock returns successive minute-ticks
// from a fixed origin so concurrent tests don't all collide on a single time.
func newTestStore(t *testing.T) *FileStore {
	t.Helper()
	origin := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	tick := 0
	clock := func() time.Time {
		tick++
		return origin.Add(time.Duration(tick) * time.Minute)
	}
	return NewFileStore(filepath.Join(t.TempDir(), "artifacts"), WithClock(clock))
}
