package dbtest

import (
	"reflect"
	"testing"
)

// FillSlices sets every exported slice field reachable from the struct that
// target points to, through nested structs and slice elements, to a
// one-element slice. It fails the test on a slice element kind it cannot
// fill, so a new kind of field is not silently skipped.
func FillSlices(t *testing.T, target any) {
	t.Helper()
	walkSlices(t, reflect.ValueOf(target).Elem(), func(field reflect.Value) {
		value := reflect.MakeSlice(field.Type(), 1, 1)
		setElement(t, value.Index(0), 1)
		field.Set(value)
	})
}

// MutateSlices changes the first element of every non-empty exported slice
// field reachable from the struct that target points to, in place. A value
// whose slices share storage with another value changes that value too.
func MutateSlices(t *testing.T, target any) {
	t.Helper()
	walkSlices(t, reflect.ValueOf(target).Elem(), func(field reflect.Value) {
		if field.Len() > 0 {
			setElement(t, field.Index(0), 2)
		}
	})
}

func walkSlices(t *testing.T, value reflect.Value, visit func(reflect.Value)) {
	t.Helper()
	switch value.Kind() {
	case reflect.Struct:
		for n := range value.NumField() {
			if value.Type().Field(n).IsExported() {
				walkSlices(t, value.Field(n), visit)
			}
		}
	case reflect.Slice:
		visit(value)
		for n := range value.Len() {
			walkSlices(t, value.Index(n), visit)
		}
	case reflect.Map, reflect.Pointer, reflect.Interface, reflect.Chan, reflect.Func:
		t.Fatalf("unsupported reference field of type %s", value.Type())
	}
}

func setElement(t *testing.T, element reflect.Value, marker int) {
	t.Helper()
	switch element.Kind() {
	case reflect.Uint8:
		element.SetUint(uint64(marker))
	case reflect.String:
		element.SetString(string(rune('a' + marker)))
	case reflect.Struct:
		// Struct elements are walked for their own slice fields.
	default:
		t.Fatalf("unsupported slice element of type %s", element.Type())
	}
}
