package artifacts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/itsHabib/orchestra/internal/fsutil"
)

// FileStore persists artifacts on the local filesystem under a root directory.
// One envelope JSON file per artifact at <root>/<agent>/<key>.json. The
// dispatcher constructs one of these per run rooted at
// <workspace>/.orchestra/artifacts/.
type FileStore struct {
	root string
	now  func() time.Time
}

// Option configures a [FileStore].
type Option func(*FileStore)

// WithClock overrides the clock used to stamp the Written field. Tests pass a
// fixed clock so envelope JSON output is byte-stable.
func WithClock(now func() time.Time) Option {
	return func(s *FileStore) {
		if now != nil {
			s.now = now
		}
	}
}

// NewFileStore returns a FileStore rooted at root. The directory is created
// lazily on first Put; List/Get against a missing root return empty / not-
// found rather than erroring, matching the "freshly-spawned run" path.
func NewFileStore(root string, opts ...Option) *FileStore {
	s := &FileStore{
		root: root,
		now:  func() time.Time { return time.Now().UTC() },
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Root returns the directory the store writes into. Useful for diagnostics
// and tests; production callers don't need it.
func (s *FileStore) Root() string { return s.root }

// Put implements [Store.Put].
func (s *FileStore) Put(ctx context.Context, runID, agent, key, phase string, art Artifact) (Meta, error) {
	if err := ctx.Err(); err != nil {
		return Meta{}, err
	}
	if err := validateName("agent", agent); err != nil {
		return Meta{}, err
	}
	if err := validateName("key", key); err != nil {
		return Meta{}, err
	}
	if art.Type != TypeText && art.Type != TypeJSON {
		return Meta{}, fmt.Errorf("artifacts: type %q must be %q or %q", art.Type, TypeText, TypeJSON)
	}
	if len(art.Content) == 0 {
		return Meta{}, errors.New("artifacts: content is empty")
	}

	path := s.artifactPath(agent, key)
	if _, err := os.Stat(path); err == nil {
		return Meta{}, fmt.Errorf("%w: agent=%q key=%q", ErrAlreadyExists, agent, key)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Meta{}, fmt.Errorf("artifacts: stat %s: %w", path, err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Meta{}, fmt.Errorf("artifacts: prepare agent dir: %w", err)
	}

	now := s.now().UTC()
	env := envelope{
		Type:    art.Type,
		Phase:   phase,
		Size:    int64(len(art.Content)),
		Written: now,
		Content: append(json.RawMessage(nil), art.Content...),
	}
	// json.Marshal (not Indent) keeps the embedded RawMessage byte-stable —
	// MarshalIndent re-indents nested raw JSON and breaks round-trip.
	data, err := json.Marshal(env)
	if err != nil {
		return Meta{}, fmt.Errorf("artifacts: marshal envelope: %w", err)
	}
	if err := fsutil.AtomicWrite(path, data); err != nil {
		return Meta{}, fmt.Errorf("artifacts: write %s: %w", path, err)
	}
	return Meta{
		RunID:   runID,
		Agent:   agent,
		Phase:   phase,
		Key:     key,
		Type:    art.Type,
		Size:    env.Size,
		Written: now,
	}, nil
}

// Get implements [Store.Get].
func (s *FileStore) Get(ctx context.Context, runID, agent, key string) (Artifact, Meta, error) {
	if err := ctx.Err(); err != nil {
		return Artifact{}, Meta{}, err
	}
	if err := validateName("agent", agent); err != nil {
		return Artifact{}, Meta{}, err
	}
	if err := validateName("key", key); err != nil {
		return Artifact{}, Meta{}, err
	}
	env, err := s.readEnvelope(s.artifactPath(agent, key))
	if err != nil {
		return Artifact{}, Meta{}, err
	}
	return Artifact{
			Type:    env.Type,
			Content: append(json.RawMessage(nil), env.Content...),
		}, Meta{
			RunID:   runID,
			Agent:   agent,
			Phase:   env.Phase,
			Key:     key,
			Type:    env.Type,
			Size:    env.Size,
			Written: env.Written,
		}, nil
}

// List implements [Store.List].
func (s *FileStore) List(ctx context.Context, runID, agent string) ([]Meta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if agent != "" {
		return s.listAgent(runID, agent)
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("artifacts: read root: %w", err)
	}
	var out []Meta
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		metas, err := s.listAgent(runID, e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, metas...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Agent != out[j].Agent {
			return out[i].Agent < out[j].Agent
		}
		return out[i].Key < out[j].Key
	})
	return out, nil
}

func (s *FileStore) listAgent(runID, agent string) ([]Meta, error) {
	if err := validateName("agent", agent); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.root, agent)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("artifacts: read agent dir: %w", err)
	}
	out := make([]Meta, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		key := strings.TrimSuffix(name, ".json")
		env, err := s.readEnvelope(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		out = append(out, Meta{
			RunID:   runID,
			Agent:   agent,
			Phase:   env.Phase,
			Key:     key,
			Type:    env.Type,
			Size:    env.Size,
			Written: env.Written,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (s *FileStore) readEnvelope(path string) (envelope, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return envelope{}, ErrNotFound
		}
		return envelope{}, fmt.Errorf("artifacts: read %s: %w", path, err)
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return envelope{}, fmt.Errorf("artifacts: parse %s: %w", path, err)
	}
	return env, nil
}

func (s *FileStore) artifactPath(agent, key string) string {
	return filepath.Join(s.root, agent, key+".json")
}

// validateName rejects the path-traversal shapes that would let a stray agent
// or key value escape the store root. The schema-side validation in
// signal_completion already enforces tighter rules on agent names; this is
// defense-in-depth so a misuse of the package can't trash the workspace.
func validateName(field, name string) error {
	if name == "" {
		return fmt.Errorf("artifacts: %s is empty", field)
	}
	if name == "." || name == ".." {
		return fmt.Errorf("artifacts: %s %q is reserved", field, name)
	}
	if strings.ContainsAny(name, `/\`+"\x00") {
		return fmt.Errorf("artifacts: %s %q contains invalid characters", field, name)
	}
	if strings.HasPrefix(name, ".") {
		return fmt.Errorf("artifacts: %s %q must not start with a dot", field, name)
	}
	return nil
}
