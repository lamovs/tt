package model

import (
	"fmt"
	"strconv"
	"strings"
)

type enumValue interface{ ~int }

type enumDef[T enumValue] struct {
	val     T
	name    string
	aliases []string
}

func def[T enumValue](val T, name string, aliases ...string) enumDef[T] {
	return enumDef[T]{val: val, name: name, aliases: aliases}
}

type enum[T enumValue] struct {
	kind   string
	names  []string
	vals   []T
	byName map[string]T
	byVal  map[T]string
}

func newEnum[T enumValue](kind string, defs ...enumDef[T]) *enum[T] {
	e := &enum[T]{
		kind:   kind,
		byName: make(map[string]T, len(defs)),
		byVal:  make(map[T]string, len(defs)),
	}
	for _, d := range defs {
		if _, dup := e.byVal[d.val]; dup {
			panic(fmt.Sprintf("model: enum %s: duplicate value for %q", kind, d.name))
		}
		e.names = append(e.names, d.name)
		e.vals = append(e.vals, d.val)
		e.byVal[d.val] = d.name
		for _, n := range append([]string{d.name}, d.aliases...) {
			if _, dup := e.byName[n]; dup {
				panic(fmt.Sprintf("model: enum %s: duplicate name %q", kind, n))
			}
			e.byName[n] = d.val
		}
	}
	return e
}

func (e *enum[T]) parse(s string) (T, error) {
	if v, ok := e.byName[strings.ToLower(strings.TrimSpace(s))]; ok {
		return v, nil
	}
	var zero T
	return zero, fmt.Errorf("unknown %s %q (allowed: %s)", e.kind, s, strings.Join(e.names, ", "))
}

func (e *enum[T]) name(v T) string {
	if n, ok := e.byVal[v]; ok {
		return n
	}
	return fmt.Sprintf("%s(%d)", e.kind, int(v))
}

func (e *enum[T]) list() []string { return append([]string(nil), e.names...) }

func (e *enum[T]) valid(v T) bool {
	_, ok := e.byVal[v]
	return ok
}

func fromWire[T enumValue](e *enum[T], n int) (T, error) {
	var zero T
	if v := T(n); e.valid(v) {
		return v, nil
	}
	nums := make([]string, 0, len(e.vals))
	for _, v := range e.vals {
		nums = append(nums, strconv.Itoa(int(v)))
	}
	return zero, fmt.Errorf("unknown %s value %d (allowed: %s)", e.kind, n, strings.Join(nums, ", "))
}

func marshalEnum[T enumValue](e *enum[T], v T) ([]byte, error) {
	if !e.valid(v) {
		return nil, fmt.Errorf("invalid %s value %d", e.kind, int(v))
	}
	return []byte(e.name(v)), nil
}

func unmarshalEnum[T enumValue](e *enum[T], dst *T, b []byte) error {
	v, err := e.parse(string(b))
	if err != nil {
		return err
	}
	*dst = v
	return nil
}
