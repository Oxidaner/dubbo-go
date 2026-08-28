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
	"fmt"
	"reflect"
	"strings"
	"unicode"
	"unicode/utf8"
)

// GenericTag renames a struct field in the map form used by generic invocation.
const GenericTag = "m"

// maxSquashDepth bounds squash expansion. A struct cannot contain itself by
// value, but a squashed pointer field can, so the recursion needs a stop.
const maxSquashDepth = 32

// GenericField is one struct field's m tag, resolved.
//
// This is the single source of truth for generic field naming. Three consumers
// need it — the struct-to-map direction, the map-to-struct direction, and the
// published service definition — and when each derived names independently, a
// definition could advertise a key the provider would not accept.
type GenericField struct {
	// Index is the field's index in its declaring struct.
	Index int
	// Name is the wire key: the tag name when given, otherwise the field name
	// with its first rune lowercased. Empty when Ignored.
	Name string
	// GoName is the declared field name. It remains accepted as a
	// case-insensitive alias because generic callers predating m tags send it.
	GoName string
	// DecodeKey is the key mapstructure derives for this field: the tag text
	// before the comma when non-empty, the Go name otherwise. Callers driving
	// mapstructure key their normalized input by this so its lookup hits
	// exactly instead of falling back to a fuzzy scan.
	DecodeKey string
	// Type is the field's declared type.
	Type reflect.Type

	Ignored   bool // m:"-"
	OmitEmpty bool // m:",omitempty"
	Squash    bool // m:",squash"
	Remain    bool // m:",remain"
}

// GenericFieldOf resolves a single struct field's tag.
//
// Callers are responsible for skipping unexported fields; this reports what the
// name would be, not whether the field is eligible.
func GenericFieldOf(field reflect.StructField) GenericField {
	resolved := GenericField{
		Name:      LowerFirstRune(field.Name),
		GoName:    field.Name,
		DecodeKey: field.Name,
		Type:      field.Type,
	}

	name, options, hasOptions := strings.Cut(field.Tag.Get(GenericTag), ",")
	if name == "-" {
		resolved.Ignored = true
		resolved.Name = ""
		resolved.DecodeKey = "-"
		return resolved
	}
	if name != "" {
		resolved.Name = name
		resolved.DecodeKey = name
	}
	if !hasOptions {
		return resolved
	}

	for option := range strings.SplitSeq(options, ",") {
		switch option {
		case "omitempty":
			resolved.OmitEmpty = true
		case "squash":
			resolved.Squash = true
		case "remain":
			resolved.Remain = true
		}
	}
	return resolved
}

// FlatGenericField is one field as it appears in the flattened wire map.
//
// Squash hoists a nested struct's fields into its parent's key space, so the
// wire form is flat even though the Go type is not. Anything that has to agree
// with the wire form — a schema, a decoder's input — has to reason about this
// flattened view rather than the declared field list.
type FlatGenericField struct {
	// Name is the wire key in the flattened map.
	Name string
	// DecodeKey is the key mapstructure will look this field up under. It is
	// flat for the same reason Name is: mapstructure hoists squashed fields
	// into one field list before matching.
	DecodeKey string
	// GoName is the declared field name, still accepted as a legacy alias.
	GoName string
	// Path is the chain of field indices from the root struct, so callers can
	// reach the field through any squashed ancestors.
	Path []int
	Type reflect.Type

	OmitEmpty bool
	Ignored   bool
}

// FlatFields is the flattened wire shape of a struct.
type FlatFields struct {
	Fields []FlatGenericField
	// HasRemain reports whether any field collects unmatched keys. Callers that
	// filter a decoder's input must pass unmatched keys through when this is
	// set, or the remain field silently receives nothing.
	HasRemain bool
}

// FlattenGenericFields resolves a struct into the flat set of wire keys it
// produces, expanding squashed fields into the parent's key space.
//
// It fails when two fields would be reachable under one name. A field is
// reachable under its wire name and its Go name, both case-insensitively, and
// squash makes collisions easy to introduce by accident — an embedded struct's
// Name and the parent's own Name land on the same key. Neither the emitted key
// nor the decode target could then be chosen without depending on field order.
func FlattenGenericFields(t reflect.Type) (FlatFields, error) {
	var out FlatFields
	owner := make(map[string]fieldOwner)
	if err := flattenInto(t, nil, &out, owner, 0); err != nil {
		return FlatFields{}, err
	}
	return out, nil
}

// fieldOwner identifies which field already claimed a name.
//
// Identity is the index path, not the Go name: squash puts fields from
// different structs into one namespace, and two of them can easily share a Go
// name — that is precisely the collision worth catching, so it must not read as
// "the same field twice".
type fieldOwner struct {
	path    string
	display string
}

func flattenInto(t reflect.Type, path []int, out *FlatFields, owner map[string]fieldOwner, depth int) error {
	if t == nil || t.Kind() != reflect.Struct {
		return fmt.Errorf("generic field resolution needs a struct type, got %v", t)
	}
	if depth > maxSquashDepth {
		return fmt.Errorf("squash nesting exceeds %d levels at %v", maxSquashDepth, t)
	}

	for i := range t.NumField() {
		structField := t.Field(i)
		if structField.PkgPath != "" {
			// Unexported: reflect cannot read it, so it is genuinely absent
			// from the wire form rather than merely omitted.
			continue
		}

		field := GenericFieldOf(structField)
		field.Index = i
		fieldPath := append(append([]int(nil), path...), i)

		if field.Remain {
			// A remain field is a sink for unmatched keys, not a wire key of
			// its own, so it claims no name.
			out.HasRemain = true
			continue
		}

		if field.Squash {
			squashed := structField.Type
			for squashed.Kind() == reflect.Pointer {
				squashed = squashed.Elem()
			}
			if squashed.Kind() != reflect.Struct {
				return fmt.Errorf("cannot squash non-struct field %s of type %v",
					structField.Name, structField.Type)
			}
			if err := flattenInto(squashed, fieldPath, out, owner, depth+1); err != nil {
				return err
			}
			continue
		}

		claimant := fieldOwner{path: fmt.Sprint(fieldPath), display: structField.Name}
		aliases := genericAliases(field)
		if field.Ignored {
			// An ignored field still reserves its Go name so a sibling cannot
			// quietly take over a name legacy callers may still be sending.
			aliases = []string{field.GoName}
		}
		for _, alias := range aliases {
			folded := strings.ToLower(alias)
			if previous, taken := owner[folded]; taken && previous.path != claimant.path {
				return &GenericFieldConflictError{
					First:  previous.display,
					Second: claimant.display,
					Name:   alias,
				}
			}
			owner[folded] = claimant
		}

		out.Fields = append(out.Fields, FlatGenericField{
			Name:      field.Name,
			DecodeKey: field.DecodeKey,
			GoName:    field.GoName,
			Path:      fieldPath,
			Type:      structField.Type,
			OmitEmpty: field.OmitEmpty,
			Ignored:   field.Ignored,
		})
	}

	return nil
}

func genericAliases(field GenericField) []string {
	if field.Name == "" || strings.EqualFold(field.Name, field.GoName) {
		return []string{field.GoName}
	}
	return []string{field.Name, field.GoName}
}

// GenericFieldConflictError reports two fields reachable under one name.
type GenericFieldConflictError struct {
	First  string
	Second string
	Name   string
}

func (e *GenericFieldConflictError) Error() string {
	return fmt.Sprintf("fields %s and %s are both reachable as %q", e.First, e.Second, e.Name)
}

// MatchGenericField finds the field a wire key targets.
//
// Resolution order is exact wire name, then case-insensitive wire name, then
// case-insensitive Go name. The exact match wins outright so a caller using the
// advertised name is never diverted to a field that merely folds to it. The
// Go-name fallback is what keeps callers predating m tags working, since they
// relied on mapstructure's own case-insensitive matching.
//
// Ignored fields are returned when matched, so callers drop the key
// deliberately instead of letting it fall through to another field.
func MatchGenericField(key string, fields []FlatGenericField) (FlatGenericField, bool) {
	for _, field := range fields {
		if !field.Ignored && field.Name != "" && field.Name == key {
			return field, true
		}
	}
	for _, field := range fields {
		if !field.Ignored && field.Name != "" && strings.EqualFold(field.Name, key) {
			return field, true
		}
	}
	for _, field := range fields {
		if strings.EqualFold(field.GoName, key) {
			return field, true
		}
	}
	return FlatGenericField{}, false
}

// LowerFirstRune lowercases the first rune of s.
//
// Rune-based on purpose: slicing the first byte splits a multi-byte leading
// rune and yields invalid UTF-8 for a field named with a non-ASCII letter.
func LowerFirstRune(s string) string {
	if s == "" {
		return s
	}
	first, size := utf8.DecodeRuneInString(s)
	return string(unicode.ToLower(first)) + s[size:]
}
