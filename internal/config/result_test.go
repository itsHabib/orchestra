package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestValidate_PopulatesFieldPath asserts that every validator emits
// issues with a structured Field path. Table-driven across each
// validator that emits issues; covers project-level, team-scoped,
// nested, backend, and warning paths.
func TestValidate_PopulatesFieldPath(t *testing.T) {
	cases := []fieldPathCase{}
	cases = append(cases, fieldPathProjectAndBackendCases()...)
	cases = append(cases, fieldPathTeamCases()...)
	cases = append(cases, fieldPathRepositoryCases()...)
	cases = append(cases, fieldPathWarningCases()...)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertFieldPath(t, tc.cfg.Validate(), tc.wantErr, tc.wantField, tc.wantWarn, tc.wantWarnField)
		})
	}
}

type fieldPathCase struct {
	name          string
	cfg           *Config
	wantErr       []string // substring expected in the matching ConfigError
	wantField     []string // expected Field path on the matching ConfigError
	wantWarn      []string // substring expected in the matching Warning
	wantWarnField []string // expected Field path on the matching Warning
}

func fieldPathProjectAndBackendCases() []fieldPathCase {
	return []fieldPathCase{
		{
			name: "missing project name",
			cfg: &Config{
				Teams: []Team{{
					Name:  "a",
					Tasks: []Task{{Summary: "x", Details: "d", Verify: "v"}},
				}},
			},
			wantErr:   []string{"project name is required"},
			wantField: nil, // project-level → empty Field per §5.3
		},
		{
			name: "unknown backend kind",
			cfg: &Config{
				Name:    "p",
				Backend: Backend{Kind: "bogus"},
				Teams: []Team{{
					Name:  "a",
					Tasks: []Task{{Summary: "x", Details: "d", Verify: "v"}},
				}},
			},
			wantErr:   []string{"backend.kind must be"},
			wantField: []string{"backend", "kind"},
		},
	}
}

func fieldPathTeamCases() []fieldPathCase {
	return []fieldPathCase{
		{
			name: "missing task summary",
			cfg: &Config{
				Name: "p",
				Teams: []Team{{
					Name: "a",
					Tasks: []Task{
						{Summary: "ok", Details: "d", Verify: "v"},
						{Summary: "ok", Details: "d", Verify: "v"},
						{Summary: "", Details: "d", Verify: "v"},
					},
				}},
			},
			wantErr:   []string{"empty summary"},
			wantField: []string{"teams", "0", "tasks", "2", "summary"},
		},
		{
			name: "self-referencing dependency",
			cfg: &Config{
				Name: "p",
				Teams: []Team{{
					Name:      "a",
					Tasks:     []Task{{Summary: "x", Details: "d", Verify: "v"}},
					DependsOn: []string{"a"},
				}},
			},
			wantErr:   []string{"cannot depend on itself"},
			wantField: []string{"teams", "0", "depends_on"},
		},
		{
			name: "unknown dependency",
			cfg: &Config{
				Name: "p",
				Teams: []Team{{
					Name:      "a",
					Tasks:     []Task{{Summary: "x", Details: "d", Verify: "v"}},
					DependsOn: []string{"ghost"},
				}},
			},
			wantErr:   []string{"unknown team"},
			wantField: []string{"teams", "0", "depends_on"},
		},
		{
			name: "dependency cycle",
			cfg: &Config{
				Name: "p",
				Teams: []Team{
					{Name: "a", Tasks: []Task{{Summary: "x", Details: "d", Verify: "v"}}, DependsOn: []string{"b"}},
					{Name: "b", Tasks: []Task{{Summary: "y", Details: "d", Verify: "v"}}, DependsOn: []string{"a"}},
				},
			},
			wantErr:   []string{"cycle"},
			wantField: []string{"teams"},
		},
	}
}

func fieldPathRepositoryCases() []fieldPathCase {
	return []fieldPathCase{
		{
			name: "missing repository url under managed_agents",
			cfg: &Config{
				Name: "p",
				Backend: Backend{
					Kind: "managed_agents",
					ManagedAgents: &ManagedAgentsBackend{
						Repository: &RepositorySpec{URL: ""},
					},
				},
				Teams: []Team{{
					Name:  "a",
					Tasks: []Task{{Summary: "x", Details: "d", Verify: "v"}},
				}},
			},
			wantErr:   []string{"backend.managed_agents.repository.url"},
			wantField: []string{"backend", "managed_agents", "repository", "url"},
		},
	}
}

func fieldPathWarningCases() []fieldPathCase {
	return []fieldPathCase{
		{
			name: "team-size warning",
			cfg: &Config{
				Name: "p",
				Teams: []Team{{
					Name:    "a",
					Members: []Member{{Role: "1"}, {Role: "2"}, {Role: "3"}, {Role: "4"}, {Role: "5"}, {Role: "6"}},
					Tasks:   manyTasks(12),
				}},
			},
			wantWarn:      []string{"members"},
			wantWarnField: []string{"teams", "0", "members"},
		},
		{
			name: "task-quality warning (empty details)",
			cfg: &Config{
				Name: "p",
				Teams: []Team{{
					Name:  "a",
					Tasks: []Task{{Summary: "do stuff"}},
				}},
			},
			wantWarn:      []string{"empty details"},
			wantWarnField: []string{"teams", "0", "tasks", "0", "details"},
		},
		{
			name: "task-quality warning (empty verify)",
			cfg: &Config{
				Name: "p",
				Teams: []Team{{
					Name:  "a",
					Tasks: []Task{{Summary: "do stuff", Details: "d"}},
				}},
			},
			wantWarn:      []string{"empty verify"},
			wantWarnField: []string{"teams", "0", "tasks", "0", "verify"},
		},
	}
}

// manyTasks returns n tasks with non-empty details/verify so they
// don't trip the task-quality warnings during fixture construction.
func manyTasks(n int) []Task {
	out := make([]Task, n)
	for i := range out {
		out[i] = Task{Summary: "t", Details: "d", Verify: "v"}
	}
	return out
}

// assertFieldPath verifies that a [Result] contains a matching
// ConfigError or Warning with the expected Field path.
func assertFieldPath(t *testing.T, res *Result, wantErr, wantField, wantWarn, wantWarnField []string) {
	t.Helper()
	if len(wantErr) > 0 {
		match := findError(res.Errors, wantErr[0])
		if match == nil {
			t.Fatalf("no ConfigError matched %q; got: %v", wantErr[0], res.Errors)
		}
		if !reflect.DeepEqual(match.Field, wantField) {
			t.Errorf("Field = %v, want %v (message: %q)", match.Field, wantField, match.Message)
		}
	}
	if len(wantWarn) > 0 {
		match := findWarning(res.Warnings, wantWarn[0])
		if match == nil {
			t.Fatalf("no Warning matched %q; got: %v", wantWarn[0], res.Warnings)
		}
		if !reflect.DeepEqual(match.Field, wantWarnField) {
			t.Errorf("Field = %v, want %v (message: %q)", match.Field, wantWarnField, match.Message)
		}
	}
}

// TestValidate_WarningsAndErrorsCoexist asserts that warnings and
// errors are accumulated independently — a config with both soft and
// hard issues populates both slices and is invalid.
func TestValidate_WarningsAndErrorsCoexist(t *testing.T) {
	cfg := &Config{
		// project name missing → hard error
		Backend: Backend{Kind: "managed_agents"},
		Teams: []Team{{
			Name:    "a",
			Members: []Member{{Role: "dev"}}, // members under managed_agents → warning
			Tasks: []Task{
				{Summary: "x", Details: "d", Verify: "v"},
				{Summary: "x", Details: "", Verify: ""}, // empty details/verify → warnings
			},
		}},
	}
	res := cfg.Validate()
	if res.Valid() {
		t.Fatal("expected invalid result")
	}
	if len(res.Errors) == 0 {
		t.Fatal("expected at least one error")
	}
	if len(res.Warnings) == 0 {
		t.Fatal("expected at least one warning")
	}
	if res.Config != nil {
		t.Errorf("expected nil Config on invalid result, got %+v", res.Config)
	}
}

// TestValidationResult_ErrIsErrInvalidConfig asserts the sentinel-wrap
// contract: errors.Is is true for invalid configs and Err is nil for
// valid configs.
func TestValidationResult_ErrIsErrInvalidConfig(t *testing.T) {
	bad := &Config{
		Teams: []Team{{Name: "a", Tasks: []Task{{Summary: "x", Details: "d", Verify: "v"}}}},
	}
	res := bad.Validate()
	if res.Err() == nil {
		t.Fatal("invalid config: Err() returned nil")
	}
	if !errors.Is(res.Err(), ErrInvalidConfig) {
		t.Fatalf("invalid config: errors.Is(Err, ErrInvalidConfig) is false; err=%v", res.Err())
	}

	good := &Config{
		Name: "p",
		Teams: []Team{{
			Name:  "a",
			Tasks: []Task{{Summary: "x", Details: "d", Verify: "v"}},
		}},
	}
	res = good.Validate()
	if res.Err() != nil {
		t.Fatalf("valid config: Err() = %v, want nil", res.Err())
	}
}

// TestValidationResult_ErrFormatPreservesCLIByteOutput asserts that
// res.Err().Error() produces a string starting with the expected
// "validation errors:\n  - " prefix and contains each error's
// String() form. CLI output relies on this for byte parity.
func TestValidationResult_ErrFormatPreservesCLIByteOutput(t *testing.T) {
	cfg := &Config{
		Teams: []Team{{
			Name:      "a",
			Tasks:     []Task{{Summary: "x", Details: "d", Verify: "v"}},
			DependsOn: []string{"a"},
		}},
	}
	res := cfg.Validate()
	got := res.Err().Error()
	if !strings.HasPrefix(got, "validation errors:\n  - ") {
		t.Fatalf("Err() string lacks CLI prefix: %q", got)
	}
	for _, e := range res.Errors {
		if !strings.Contains(got, e.String()) {
			t.Errorf("Err() string missing %q: full=%q", e.String(), got)
		}
	}
}

// TestLoadConfig_ParseErrorReturnsErrorNotResult asserts that
// malformed YAML produces (nil, error) — the error channel is reserved
// for I/O / parse failures, not validation.
func TestLoadConfig_ParseErrorReturnsErrorNotResult(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(path, []byte(":::not yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Load(path)
	if err == nil {
		t.Fatal("expected error for malformed YAML")
	}
	if res != nil {
		t.Fatalf("expected nil result on parse error, got %+v", res)
	}
}

// TestValidate_NilConfigReturnsConfigError asserts that Validate(nil)
// returns a non-nil Result with one ConfigError entry rather than
// panicking.
func TestValidate_NilConfigReturnsConfigError(t *testing.T) {
	var cfg *Config
	res := cfg.Validate()
	if res == nil {
		t.Fatal("Validate(nil): got nil Result, want one with a ConfigError entry")
	}
	if res.Valid() {
		t.Fatal("Validate(nil): expected invalid result")
	}
	if len(res.Errors) == 0 {
		t.Fatal("Validate(nil): expected at least one ConfigError")
	}
	if !strings.Contains(res.Errors[0].Message, "nil config") {
		t.Errorf("Validate(nil): unexpected Message %q", res.Errors[0].Message)
	}
}

// TestValidate_ConfigNilWhenInvalid asserts that res.Config is nil for
// configs that fail validation, so consumers cannot accidentally hand
// an invalid config to Run.
func TestValidate_ConfigNilWhenInvalid(t *testing.T) {
	cfg := &Config{
		Teams: []Team{{Name: "a", Tasks: []Task{{Summary: "x", Details: "d", Verify: "v"}}}},
	}
	res := cfg.Validate()
	if res.Valid() {
		t.Fatal("expected invalid result")
	}
	if res.Config != nil {
		t.Errorf("expected res.Config == nil on invalid result, got %+v", res.Config)
	}
}

// findError returns the first ConfigError whose Message contains
// substr, or nil.
func findError(errs []ConfigError, substr string) *ConfigError {
	for i := range errs {
		if strings.Contains(errs[i].Message, substr) {
			return &errs[i]
		}
	}
	return nil
}

// findWarning returns the first Warning whose Message contains
// substr, or nil.
func findWarning(warns []Warning, substr string) *Warning {
	for i := range warns {
		if strings.Contains(warns[i].Message, substr) {
			return &warns[i]
		}
	}
	return nil
}
