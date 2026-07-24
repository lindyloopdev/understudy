// Package slogdiff provides test helpers for asserting on captured slog output.
package slogdiff

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"

	"github.com/google/go-cmp/cmp"
)

// A Matcher is a want-map value (for JSONContains / JSONCount) that decides
// whether a record's attr satisfies an expectation instead of comparing by
// equality. present reports whether the record carried the attr at all.
type Matcher interface {
	Match(value any, present bool) bool
}

type substrMatcher string

func (s substrMatcher) Match(value any, _ bool) bool {
	str, ok := value.(string)
	return ok && strings.Contains(str, string(s))
}

// Substr returns a Matcher satisfied when the attr is a string containing s —
// for attrs whose exact value is an opaque wrapped message.
func Substr(s string) Matcher { return substrMatcher(s) }

type absentMatcher struct{}

func (absentMatcher) Match(_ any, present bool) bool { return !present }

// Absent returns a Matcher satisfied when the record does not carry the attr.
func Absent() Matcher { return absentMatcher{} }

// recordMatches reports whether record satisfies every key in want: a Matcher
// value is applied as a predicate, any other value is compared by equality.
func recordMatches(record, want map[string]any) bool {
	for k, v := range want {
		got, present := record[k]
		if m, ok := v.(Matcher); ok {
			if !m.Match(got, present) {
				return false
			}
			continue
		}
		if !reflect.DeepEqual(got, v) {
			return false
		}
	}
	return true
}

// JSONContains reports whether raw (a stream of JSON slog records, one object per
// line) contains a record matching want: every key in want must be present in
// the record with an equal value, or satisfy its Matcher. Panics if raw contains
// malformed JSON.
func JSONContains(raw []byte, want map[string]any) bool {
	dec := json.NewDecoder(bytes.NewReader(raw))
	for {
		var record map[string]any
		if err := dec.Decode(&record); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			panic(err)
		}
		if recordMatches(record, want) {
			return true
		}
	}
	return false
}

// JSONCount reports how many records in raw (a stream of JSON slog records, one
// object per line) match want: every key in want must be present in the record
// with an equal value, or satisfy its Matcher. Panics if raw contains malformed
// JSON.
func JSONCount(raw []byte, want map[string]any) int {
	dec := json.NewDecoder(bytes.NewReader(raw))
	count := 0
	for {
		var record map[string]any
		if err := dec.Decode(&record); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			panic(err)
		}
		if recordMatches(record, want) {
			count++
		}
	}
	return count
}

// JSONLines parses raw as a stream of JSON slog records, decoding each into
// map[string]any, then returns cmp.Diff(want, got, opts...). Empty string
// means match. Panics if raw contains malformed JSON.
func JSONLines(want []map[string]any, raw []byte, opts ...cmp.Option) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	var got []map[string]any
	for {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			panic(err)
		}
		got = append(got, m)
	}
	return cmp.Diff(want, got, opts...)
}
