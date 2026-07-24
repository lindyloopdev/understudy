package slogdiff_test

import (
	"testing"

	"github.com/google/go-cmp/cmp/cmpopts"
	"gitlab.com/flimzy/testy/v2"

	"github.com/lindyloopdev/understudy/internal/slogdiff"
)

func TestJSONLinesPanicsOnMalformedJSON(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on malformed JSON, got none")
		}
	}()
	slogdiff.JSONLines(nil, []byte("not json"))
}

func TestJSONContainsPanicsOnMalformedJSON(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on malformed JSON, got none")
		}
	}()
	slogdiff.JSONContains([]byte("not json"), nil)
}

func TestJSONCountPanicsOnMalformedJSON(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on malformed JSON, got none")
		}
	}()
	slogdiff.JSONCount([]byte("not json"), nil)
}

func TestJSONLines(t *testing.T) {
	t.Parallel()

	type test struct {
		raw       []byte
		want      []map[string]any
		wantEmpty bool
	}

	ignoreTime := cmpopts.IgnoreMapEntries(func(k string, _ any) bool {
		return k == "time"
	})

	tests := testy.NewTable[test]()

	tests.Add("should return empty diff when both raw and want are empty", test{
		wantEmpty: true,
	})

	tests.Add("should match two slog records ignoring the JSONHandler time field", test{
		raw: []byte(
			`{"time":"2024-01-01T00:00:00Z","level":"INFO","msg":"request","status":200}` + "\n" +
				`{"time":"2024-01-01T00:00:01Z","level":"INFO","msg":"request","status":500}` + "\n",
		),
		want: []map[string]any{
			{"level": "INFO", "msg": "request", "status": float64(200)},
			{"level": "INFO", "msg": "request", "status": float64(500)},
		},
		wantEmpty: true,
	})

	tests.Add("should ignore extra whitespace between, before, and after records", test{
		raw: []byte(
			"\n\n  " +
				`{"time":"2024-01-01T00:00:00Z","level":"INFO","msg":"request","status":200}` +
				"\n\n   \n" +
				`{"time":"2024-01-01T00:00:01Z","level":"INFO","msg":"request","status":500}` +
				"\n  \n",
		),
		want: []map[string]any{
			{"level": "INFO", "msg": "request", "status": float64(200)},
			{"level": "INFO", "msg": "request", "status": float64(500)},
		},
		wantEmpty: true,
	})

	tests.Add("should return a non-empty diff when records appear in a different order", test{
		raw: []byte(
			`{"time":"2024-01-01T00:00:00Z","level":"INFO","msg":"request","status":200}` + "\n" +
				`{"time":"2024-01-01T00:00:01Z","level":"INFO","msg":"request","status":500}` + "\n",
		),
		want: []map[string]any{
			{"level": "INFO", "msg": "request", "status": float64(500)},
			{"level": "INFO", "msg": "request", "status": float64(200)},
		},
		wantEmpty: false,
	})

	tests.Add("should return a non-empty diff when records do not match", test{
		raw: []byte(
			`{"time":"2024-01-01T00:00:00Z","level":"INFO","msg":"request","status":200}` + "\n" +
				`{"time":"2024-01-01T00:00:01Z","level":"INFO","msg":"request","status":500}` + "\n",
		),
		want: []map[string]any{
			{"level": "INFO", "msg": "request", "status": float64(999)},
			{"level": "INFO", "msg": "request", "status": float64(999)},
		},
		wantEmpty: false,
	})

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		got := slogdiff.JSONLines(tt.want, tt.raw, ignoreTime)
		if (got == "") != tt.wantEmpty {
			t.Errorf("JSONLines() empty=%v, want empty=%v; diff:\n%s", got == "", tt.wantEmpty, got)
		}
	})
}

func TestContains(t *testing.T) {
	t.Parallel()

	type test struct {
		raw   []byte
		match map[string]any
		want  bool
	}

	tests := testy.NewTable[test]()

	tests.Add("should report true when a record matches all given fields", test{
		raw: []byte(
			`{"time":"2024-01-01T00:00:00Z","level":"INFO","msg":"request","status":200}` + "\n" +
				`{"time":"2024-01-01T00:00:01Z","level":"INFO","msg":"request","status":500}` + "\n",
		),
		match: map[string]any{"msg": "request", "status": float64(500)},
		want:  true,
	})

	tests.Add("should report false when no record matches every field", test{
		raw: []byte(
			`{"time":"2024-01-01T00:00:00Z","level":"INFO","msg":"request","status":200}` + "\n" +
				`{"time":"2024-01-01T00:00:01Z","level":"INFO","msg":"request","status":500}` + "\n",
		),
		match: map[string]any{"msg": "request", "status": float64(404)},
		want:  false,
	})

	tests.Add("should match a string attr containing the substring when the want value is a Substr matcher", test{
		raw:   []byte(`{"time":"2024-01-01T00:00:00Z","level":"INFO","msg":"request","error":"upstream returned status 500: boom"}` + "\n"),
		match: map[string]any{"msg": "request", "error": slogdiff.Substr("boom")},
		want:  true,
	})

	tests.Add("should match a record lacking the attr when the want value is an Absent matcher", test{
		raw:   []byte(`{"time":"2024-01-01T00:00:00Z","level":"INFO","msg":"request","status":200}` + "\n"),
		match: map[string]any{"msg": "request", "error": slogdiff.Absent()},
		want:  true,
	})

	tests.Add("should not match a record carrying the attr when the want value is an Absent matcher", test{
		raw:   []byte(`{"time":"2024-01-01T00:00:00Z","level":"INFO","msg":"request","error":"boom"}` + "\n"),
		match: map[string]any{"msg": "request", "error": slogdiff.Absent()},
		want:  false,
	})

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		got := slogdiff.JSONContains(tt.raw, tt.match)
		if got != tt.want {
			t.Errorf("JSONContains() = %v, want %v", got, tt.want)
		}
	})
}

func TestJSONCount(t *testing.T) {
	t.Parallel()

	type test struct {
		raw   []byte
		match map[string]any
		want  int
	}

	tests := testy.NewTable[test]()

	tests.Add("should count every record matching the subset", test{
		raw: []byte(
			`{"time":"2024-01-01T00:00:00Z","level":"INFO","msg":"request","status":500}` + "\n" +
				`{"time":"2024-01-01T00:00:01Z","level":"INFO","msg":"request","status":200}` + "\n" +
				`{"time":"2024-01-01T00:00:02Z","level":"INFO","msg":"request","status":500}` + "\n",
		),
		match: map[string]any{"status": float64(500)},
		want:  2,
	})

	tests.Add("should count exactly one record matching the subset", test{
		raw: []byte(
			`{"time":"2024-01-01T00:00:00Z","level":"INFO","msg":"request","status":200}` + "\n" +
				`{"time":"2024-01-01T00:00:01Z","level":"INFO","msg":"request","status":500}` + "\n",
		),
		match: map[string]any{"status": float64(500)},
		want:  1,
	})

	tests.Add("should return zero when no record matches the subset", test{
		raw: []byte(
			`{"time":"2024-01-01T00:00:00Z","level":"INFO","msg":"request","status":200}` + "\n" +
				`{"time":"2024-01-01T00:00:01Z","level":"INFO","msg":"request","status":500}` + "\n",
		),
		match: map[string]any{"status": float64(404)},
		want:  0,
	})

	tests.Add("should not count a record matching only some of the wanted fields", test{
		raw: []byte(
			`{"time":"2024-01-01T00:00:00Z","level":"INFO","msg":"request","status":500}` + "\n" +
				`{"time":"2024-01-01T00:00:01Z","level":"INFO","msg":"other","status":500}` + "\n" +
				`{"time":"2024-01-01T00:00:02Z","level":"INFO","msg":"request","status":200}` + "\n",
		),
		match: map[string]any{"msg": "request", "status": float64(500)},
		want:  1,
	})

	tests.Add("should count every record when want is empty", test{
		raw: []byte(
			`{"time":"2024-01-01T00:00:00Z","level":"INFO","msg":"request","status":200}` + "\n" +
				`{"time":"2024-01-01T00:00:01Z","level":"INFO","msg":"request","status":500}` + "\n",
		),
		match: nil,
		want:  2,
	})

	tests.Add("should count only records whose attr contains the substring when given a Substr matcher", test{
		raw: []byte(
			`{"time":"2024-01-01T00:00:00Z","level":"INFO","msg":"request","error":"upstream returned status 500: boom"}` + "\n" +
				`{"time":"2024-01-01T00:00:01Z","level":"INFO","msg":"request","error":"context deadline exceeded"}` + "\n" +
				`{"time":"2024-01-01T00:00:02Z","level":"INFO","msg":"request","error":"downstream boom again"}` + "\n",
		),
		match: map[string]any{"error": slogdiff.Substr("boom")},
		want:  2,
	})

	tests.Add("should count only records lacking the attr when given an Absent matcher", test{
		raw: []byte(
			`{"time":"2024-01-01T00:00:00Z","level":"INFO","msg":"request","status":200}` + "\n" +
				`{"time":"2024-01-01T00:00:01Z","level":"INFO","msg":"request","status":500,"error":"boom"}` + "\n" +
				`{"time":"2024-01-01T00:00:02Z","level":"INFO","msg":"request","status":204}` + "\n",
		),
		match: map[string]any{"error": slogdiff.Absent()},
		want:  2,
	})

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		got := slogdiff.JSONCount(tt.raw, tt.match)
		if got != tt.want {
			t.Errorf("JSONCount() = %v, want %v", got, tt.want)
		}
	})
}
