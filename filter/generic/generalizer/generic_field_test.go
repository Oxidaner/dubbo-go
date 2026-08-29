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

package generalizer

import (
	"reflect"
	"sort"
	"testing"
)

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

import (
	"dubbo.apache.org/dubbo-go/v3/common/dubboutil"
)

// profile exercises the field shapes the resolver handles.
type profile struct {
	Name    string // untagged: wire name is "name"
	Zip     string `m:"zipCode"` // tagged
	Secret  string `m:"-"`       // ignored in both directions
	Ünicode string // wire name lowercases the first rune
	hidden  string //nolint:unused // unexported: never on the wire
}

func generalizeMap(t *testing.T, obj any) map[string]any {
	t.Helper()
	out, err := GetMapGeneralizer().Generalize(obj)
	require.NoError(t, err)
	m, ok := out.(map[string]any)
	require.True(t, ok, "expected a map, got %T", out)
	return m
}

func realizeProfile(t *testing.T, m map[string]any) (profile, error) {
	t.Helper()
	out, err := GetMapGeneralizer().Realize(m, reflect.TypeFor[profile]())
	if err != nil {
		return profile{}, err
	}
	return out.(profile), nil
}

func sortedMapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---------------------------------------------------------------------------
// the resolver drives both directions
// ---------------------------------------------------------------------------

// TestGeneralizeKeysMatchTheResolver is the invariant the shared resolver
// exists for. Generalize walks the struct recursively while the resolver
// flattens it up front; they must still agree on the exact key set, or a
// service definition built from the resolver would describe keys the wire does
// not carry.
func TestGeneralizeKeysMatchTheResolver(t *testing.T) {
	for _, tc := range []struct {
		name string
		obj  any
		typ  reflect.Type
	}{
		{"plain", profile{}, reflect.TypeFor[profile]()},
		{"squashed", squashedOuter{}, reflect.TypeFor[squashedOuter]()},
		{"nested squash", deeplySquashed{}, reflect.TypeFor[deeplySquashed]()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flat, err := dubboutil.FlattenGenericFields(tc.typ)
			require.NoError(t, err)

			want := make([]string, 0, len(flat.Fields))
			for _, field := range flat.Fields {
				if field.Ignored {
					continue
				}
				want = append(want, field.Name)
			}
			sort.Strings(want)

			assert.Equal(t, want, sortedMapKeys(generalizeMap(t, tc.obj)))
		})
	}
}

func TestGeneralizeEmitsWireNames(t *testing.T) {
	m := generalizeMap(t, profile{
		Name: "ada", Zip: "12345", Secret: "hunter2", Ünicode: "yes", hidden: "no",
	})

	assert.Equal(t, "ada", m["name"], "untagged field lowercases its first rune")
	assert.Equal(t, "12345", m["zipCode"], "the m tag wins")
	assert.Equal(t, "yes", m["ünicode"])

	assert.NotContains(t, m, "Name", "the Go name is not the wire name")
	assert.NotContains(t, m, "Secret")
	assert.NotContains(t, m, "hidden")
	assert.NotContains(t, m, "-", `m:"-" must not be emitted as a field named "-"`)
}

func TestRealizeAcceptsWireNames(t *testing.T) {
	got, err := realizeProfile(t, map[string]any{
		"name": "ada", "zipCode": "12345", "ünicode": "yes",
	})
	require.NoError(t, err)

	assert.Equal(t, "ada", got.Name)
	assert.Equal(t, "12345", got.Zip)
	assert.Equal(t, "yes", got.Ünicode)
}

// TestGenericRoundTrip is the property that matters: whatever Generalize emits,
// Realize accepts back into the same value.
func TestGenericRoundTrip(t *testing.T) {
	original := profile{Name: "ada", Zip: "12345", Ünicode: "yes"}

	got, err := realizeProfile(t, generalizeMap(t, original))
	require.NoError(t, err)
	assert.Equal(t, original, got)
}

func TestGenericRoundTripDropsIgnoredField(t *testing.T) {
	got, err := realizeProfile(t, generalizeMap(t, profile{Name: "ada", Secret: "hunter2"}))
	require.NoError(t, err)
	assert.Empty(t, got.Secret)
}

// ---------------------------------------------------------------------------
// legacy compatibility
// ---------------------------------------------------------------------------

// TestRealizeAcceptsLegacyGoNames keeps generic callers written before m tags
// existed working. They relied on mapstructure matching the Go field name
// case-insensitively.
func TestRealizeAcceptsLegacyGoNames(t *testing.T) {
	for name, input := range map[string]map[string]any{
		"exact Go name":      {"Zip": "12345"},
		"lowercased Go name": {"zip": "12345"},
		"uppercased Go name": {"ZIP": "12345"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := realizeProfile(t, input)
			require.NoError(t, err)
			assert.Equal(t, "12345", got.Zip)
		})
	}
}

// TestRealizeRejectsAmbiguousInput covers what the old decoder resolved by map
// iteration order.
func TestRealizeRejectsAmbiguousInput(t *testing.T) {
	_, err := realizeProfile(t, map[string]any{
		"zipCode": "wire name",
		"Zip":     "legacy name",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "both target field")
}

// TestRealizeWillNotWriteIgnoredFieldByGoName closes the back door: an ignored
// field must not be settable under a name the resolver refused to publish.
func TestRealizeWillNotWriteIgnoredFieldByGoName(t *testing.T) {
	for _, key := range []string{"Secret", "secret", "-"} {
		t.Run(key, func(t *testing.T) {
			got, err := realizeProfile(t, map[string]any{key: "hunter2"})
			require.NoError(t, err)
			assert.Empty(t, got.Secret)
		})
	}
}

// TestRealizeIgnoresUnknownKeys preserves existing behavior. Callers routinely
// send extras — "class" among them — and rejecting those would break working
// providers.
func TestRealizeIgnoresUnknownKeys(t *testing.T) {
	got, err := realizeProfile(t, map[string]any{
		"name": "ada", "class": "org.example.Profile", "whatever": 42,
	})
	require.NoError(t, err)
	assert.Equal(t, "ada", got.Name)
}

func TestRealizeAcceptsInterfaceKeyedMaps(t *testing.T) {
	// The shape hessian-decoded payloads and objToMap's map branch produce.
	out, err := GetMapGeneralizer().Realize(
		map[any]any{"name": "ada", "zipCode": "12345"}, reflect.TypeFor[profile]())
	require.NoError(t, err)

	got := out.(profile)
	assert.Equal(t, "ada", got.Name)
	assert.Equal(t, "12345", got.Zip)
}

func TestNestedStructsUseTheSameRules(t *testing.T) {
	type container struct {
		Inner  profile
		Nested *profile
	}
	original := container{
		Inner:  profile{Name: "inner", Zip: "1"},
		Nested: &profile{Name: "nested", Zip: "2"},
	}

	m := generalizeMap(t, original)
	inner, ok := m["inner"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "1", inner["zipCode"], "the hook must fire at every nesting level")

	out, err := GetMapGeneralizer().Realize(m, reflect.TypeFor[container]())
	require.NoError(t, err)
	got := out.(container)
	assert.Equal(t, original.Inner, got.Inner)
	require.NotNil(t, got.Nested)
	assert.Equal(t, *original.Nested, *got.Nested)
}

// ---------------------------------------------------------------------------
// squash
// ---------------------------------------------------------------------------

type squashedInner struct {
	Code string
	Kind string `m:"kind"`
}

type squashedOuter struct {
	Inner squashedInner `m:",squash"`
	Name  string
}

type deeplySquashed struct {
	Mid  squashedOuter `m:",squash"`
	Leaf string
}

func TestSquashRoundTrip(t *testing.T) {
	original := squashedOuter{
		Inner: squashedInner{Code: "c", Kind: "k"},
		Name:  "n",
	}

	m := generalizeMap(t, original)
	assert.Equal(t, []string{"code", "kind", "name"}, sortedMapKeys(m),
		"squash flattens into the parent's key space")

	out, err := GetMapGeneralizer().Realize(m, reflect.TypeFor[squashedOuter]())
	require.NoError(t, err)
	assert.Equal(t, original, out.(squashedOuter))
}

func TestNestedSquashRoundTrip(t *testing.T) {
	original := deeplySquashed{
		Mid:  squashedOuter{Inner: squashedInner{Code: "c", Kind: "k"}, Name: "n"},
		Leaf: "l",
	}

	m := generalizeMap(t, original)
	assert.Equal(t, []string{"code", "kind", "leaf", "name"}, sortedMapKeys(m))

	out, err := GetMapGeneralizer().Realize(m, reflect.TypeFor[deeplySquashed]())
	require.NoError(t, err)
	assert.Equal(t, original, out.(deeplySquashed))
}

func TestSquashAcceptsLegacyGoNames(t *testing.T) {
	// A squashed field is still reachable under its Go name, like any other.
	out, err := GetMapGeneralizer().Realize(
		map[string]any{"Code": "c", "Kind": "k", "Name": "n"},
		reflect.TypeFor[squashedOuter]())
	require.NoError(t, err)

	got := out.(squashedOuter)
	assert.Equal(t, "c", got.Inner.Code)
	assert.Equal(t, "k", got.Inner.Kind)
	assert.Equal(t, "n", got.Name)
}

func TestSquashCollisionRejectedInBothDirections(t *testing.T) {
	type colliding struct {
		Inner squashedInner `m:",squash"`
		Code  string
	}

	_, err := GetMapGeneralizer().Generalize(colliding{})
	require.Error(t, err, "the flattened key space has two fields named code")

	_, err = GetMapGeneralizer().Realize(
		map[string]any{"code": "c"}, reflect.TypeFor[colliding]())
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// remain
// ---------------------------------------------------------------------------

type withRemain struct {
	Name  string
	Extra map[string]any `m:",remain"`
}

// TestRemainStillCollectsUnmatchedKeys guards the interaction between the
// normalizing hook and mapstructure's remain support: the hook drops unmatched
// keys, which would leave a remain field permanently empty if it did not make
// an exception for them.
func TestRemainStillCollectsUnmatchedKeys(t *testing.T) {
	out, err := GetMapGeneralizer().Realize(
		map[string]any{"name": "ada", "unknown": "value", "another": 42},
		reflect.TypeFor[withRemain]())
	require.NoError(t, err)

	got := out.(withRemain)
	assert.Equal(t, "ada", got.Name)
	assert.Equal(t, "value", got.Extra["unknown"])
	assert.Equal(t, 42, got.Extra["another"])
	assert.NotContains(t, got.Extra, "name", "matched keys are not left over")
}

func TestRemainFieldIsNotGeneralized(t *testing.T) {
	// It holds whatever was left over on the way in; emitting it as a key would
	// re-nest keys that belong at the top level.
	m := generalizeMap(t, withRemain{Name: "ada", Extra: map[string]any{"x": 1}})
	assert.Equal(t, []string{"name"}, sortedMapKeys(m))
}
