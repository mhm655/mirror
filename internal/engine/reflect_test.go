package engine

import (
	"fmt"
	"reflect"

	"github.com/mirror-sim/mirror/internal/state"
)

// findFloatFields walks state.State and reports any float-typed field,
// including inside nested structs and slice/array element types.
//
// Why this exists: MIRROR's determinism guarantee rests on integer arithmetic.
// Go's float64 arithmetic is IEEE-754 and therefore reproducible in principle,
// but the compiler is permitted to contract a*b+c into an FMA on architectures
// that have one, which changes the result -- so a float in state is a
// cross-architecture divergence waiting to happen. Catching it with reflection
// is cheap; catching it from a customer-reported replay mismatch is not.
func findFloatFields() []string {
	var bad []string
	seen := map[reflect.Type]bool{}
	var walk func(t reflect.Type, path string)
	walk = func(t reflect.Type, path string) {
		if seen[t] {
			return
		}
		seen[t] = true
		switch t.Kind() {
		case reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128:
			bad = append(bad, fmt.Sprintf("%s (%s)", path, t))
		case reflect.Slice, reflect.Array, reflect.Ptr:
			walk(t.Elem(), path+"[]")
		case reflect.Map:
			walk(t.Key(), path+".key")
			walk(t.Elem(), path+".value")
		case reflect.Struct:
			for i := 0; i < t.NumField(); i++ {
				f := t.Field(i)
				walk(f.Type, path+"."+f.Name)
			}
		}
	}
	walk(reflect.TypeOf(state.State{}), "State")
	return bad
}
