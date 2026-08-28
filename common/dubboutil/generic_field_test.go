/*
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package dubboutil

import (
	"reflect"
	"testing"
	"unicode/utf8"
)

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type tagged struct {
	Plain     string
	Renamed   string `m:"renamed"`
	Dropped   string `m:"-"`
	Optional  string `m:"optional,omitempty"`
	Ünicode   string
	unexposed string //nolint:unused // asserts unexported fields are skipped
}

func flatByName(t *testing.T, typ reflect.Type) map[string]FlatGenericField {
	t.Helper()
	flat, err := FlattenGenericFields(typ)
	require.NoError(t, err)

	out := make(map[string]FlatGenericField, len(flat.Fields))
	for _, f := range flat.Fields {
		out[f.GoName] = f
	}
	return out
}

func TestGenericFieldOfNaming(t *testing.T) {
	fields := flatByName(t, reflect.TypeFor[tagged]())

	assert.Equal(t, "plain", fields["Plain"].Name, "untagged fields lowercase the first rune")
	assert.Equal(t, "Plain", fields["Plain"].DecodeKey,
		"mapstructure derives the Go name for an untagged field")

	assert.Equal(t, "renamed", fields["Renamed"].Name)
	assert.Equal(t, "renamed", fields["Renamed"].DecodeKey)

	assert.True(t, fields["Dropped"].Ignored)
	assert.Empty(t, fields["Dropped"].Name, "an ignored field has no wire name")

	// An option changes whether a value is sent, not what it is called.
	assert.Equal(t, "optional", fields["Optional"].Name)
	assert.Equal(t, "optional", fields["Optional"].DecodeKey)
	assert.True(t, fields["Optional"].OmitEmpty)

	assert.NotContains(t, fields, "unexposed", "unexported fields never reach the wire")
}

func TestGenericFieldOfLowercasesByRune(t *testing.T) {
	// Slicing the first byte splits a multi-byte rune and yields invalid UTF-8.
	name := flatByName(t, reflect.TypeFor[tagged]())["Ünicode"].Name
	assert.Equal(t, "ünicode", name)
	assert.True(t, utf8.ValidString(name))
}

func TestLowerFirstRune(t *testing.T) {
	cases := map[string]string{
		"":        "",
		"A":       "a",
		"Name":    "name",
		"name":    "name",
		"Ünicode": "ünicode",
		"日本語":     "日本語", // no lowercase form; must survive unchanged
		"ID":      "iD",  // only the first rune is touched, as before
	}
	for in, want := range cases {
		got := LowerFirstRune(in)
		assert.Equal(t, want, got, "LowerFirstRune(%q)", in)
		assert.True(t, utf8.ValidString(got))
	}
}

// ---------------------------------------------------------------------------
// squash flattening
// ---------------------------------------------------------------------------

type inner struct {
	Code string
	Kind string `m:"kind"`
}

type outer struct {
	Inner inner `m:",squash"`
	Name  string
}

type deepOuter struct {
	Mid  outer `m:",squash"`
	Leaf string
}

func TestFlattenExpandsSquash(t *testing.T) {
	flat, err := FlattenGenericFields(reflect.TypeFor[outer]())
	require.NoError(t, err)

	names := make([]string, 0, len(flat.Fields))
	for _, f := range flat.Fields {
		names = append(names, f.Name)
	}
	assert.ElementsMatch(t, []string{"code", "kind", "name"}, names,
		"squashed fields belong to the parent's key space, not a nested object")
}

func TestFlattenRecordsPathThroughSquash(t *testing.T) {
	flat, err := FlattenGenericFields(reflect.TypeFor[outer]())
	require.NoError(t, err)

	byName := make(map[string]FlatGenericField, len(flat.Fields))
	for _, f := range flat.Fields {
		byName[f.Name] = f
	}
	assert.Equal(t, []int{0, 0}, byName["code"].Path, "reachable through the squashed field")
	assert.Equal(t, []int{1}, byName["name"].Path)
}

func TestFlattenHandlesNestedSquash(t *testing.T) {
	flat, err := FlattenGenericFields(reflect.TypeFor[deepOuter]())
	require.NoError(t, err)

	names := make([]string, 0, len(flat.Fields))
	for _, f := range flat.Fields {
		names = append(names, f.Name)
	}
	assert.ElementsMatch(t, []string{"code", "kind", "name", "leaf"}, names)
}

func TestFlattenSquashThroughPointer(t *testing.T) {
	type ptrOuter struct {
		Inner *inner `m:",squash"`
		Name  string
	}
	flat, err := FlattenGenericFields(reflect.TypeFor[ptrOuter]())
	require.NoError(t, err)
	assert.Len(t, flat.Fields, 3)
}

func TestFlattenRejectsNonStructSquash(t *testing.T) {
	_, err := FlattenGenericFields(reflect.TypeFor[struct {
		Value string `m:",squash"`
	}]())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot squash non-struct")
}

// ---------------------------------------------------------------------------
// conflicts
// ---------------------------------------------------------------------------

func TestFlattenRejectsAmbiguousFields(t *testing.T) {
	cases := map[string]reflect.Type{
		"tag collides with another field's Go name": reflect.TypeFor[struct {
			Name  string
			Other string `m:"name"`
		}](),
		"two tags collide": reflect.TypeFor[struct {
			A string `m:"same"`
			B string `m:"Same"`
		}](),
		// Squash puts two structs' fields in one namespace, which is the easiest
		// way to introduce a collision by accident.
		"squashed field collides with the parent": reflect.TypeFor[struct {
			Inner inner `m:",squash"`
			Code  string
		}](),
		"two squashed structs collide": reflect.TypeFor[struct {
			A inner `m:",squash"`
			B inner `m:",squash"`
		}](),
	}

	for name, typ := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := FlattenGenericFields(typ)
			require.Error(t, err)

			var target *GenericFieldConflictError
			assert.ErrorAs(t, err, &target)
		})
	}
}

func TestFlattenAllowsTagMatchingOwnName(t *testing.T) {
	// A tag restating the field's own name is not a collision with itself.
	_, err := FlattenGenericFields(reflect.TypeFor[struct {
		Name string `m:"Name"`
	}]())
	assert.NoError(t, err)
}

func TestIgnoredFieldStillReservesItsGoName(t *testing.T) {
	// Otherwise a sibling could quietly claim a name legacy callers are still
	// sending to the ignored field.
	_, err := FlattenGenericFields(reflect.TypeFor[struct {
		Secret string `m:"-"`
		Other  string `m:"secret"`
	}]())
	require.Error(t, err)

	var target *GenericFieldConflictError
	assert.ErrorAs(t, err, &target)
}

// ---------------------------------------------------------------------------
// remain
// ---------------------------------------------------------------------------

func TestFlattenReportsRemain(t *testing.T) {
	flat, err := FlattenGenericFields(reflect.TypeFor[struct {
		Name  string
		Extra map[string]any `m:",remain"`
	}]())
	require.NoError(t, err)

	assert.True(t, flat.HasRemain)
	assert.Len(t, flat.Fields, 1, "a remain field is a sink, not a wire key of its own")
	assert.Equal(t, "name", flat.Fields[0].Name)
}

func TestFlattenWithoutRemain(t *testing.T) {
	flat, err := FlattenGenericFields(reflect.TypeFor[tagged]())
	require.NoError(t, err)
	assert.False(t, flat.HasRemain)
}

// ---------------------------------------------------------------------------
// matching
// ---------------------------------------------------------------------------

func TestMatchGenericField(t *testing.T) {
	flat, err := FlattenGenericFields(reflect.TypeFor[tagged]())
	require.NoError(t, err)

	cases := []struct {
		key      string
		wantGo   string
		wantFind bool
	}{
		{"renamed", "Renamed", true}, // exact wire name
		{"RENAMED", "Renamed", true}, // case-insensitive wire name
		{"plain", "Plain", true},     // exact wire name
		{"Plain", "Plain", true},     // legacy Go name
		{"PLAIN", "Plain", true},     // legacy Go name, case-insensitive
		{"Dropped", "Dropped", true}, // ignored, matched so callers can drop it
		{"nonexistent", "", false},
		{"-", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			field, found := MatchGenericField(tc.key, flat.Fields)
			assert.Equal(t, tc.wantFind, found)
			if tc.wantFind {
				assert.Equal(t, tc.wantGo, field.GoName)
			}
		})
	}
}

func TestMatchGenericFieldPrefersExactWireName(t *testing.T) {
	// One field's wire name folding into another's Go name must not divert a
	// caller using the advertised name.
	flat, err := FlattenGenericFields(reflect.TypeFor[struct {
		First  string `m:"target"`
		Second string `m:"other"`
	}]())
	require.NoError(t, err)

	field, found := MatchGenericField("target", flat.Fields)
	require.True(t, found)
	assert.Equal(t, "First", field.GoName)
}

func TestFlattenRejectsNonStruct(t *testing.T) {
	for _, typ := range []reflect.Type{
		reflect.TypeFor[string](),
		reflect.TypeFor[map[string]string](),
		nil,
	} {
		_, err := FlattenGenericFields(typ)
		assert.Error(t, err)
	}
}
