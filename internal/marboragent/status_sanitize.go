package marboragent

import "reflect"

// withoutNonFinite returns a deep copy of v in which every non-finite float
// has been dropped: a *float64 holding NaN/Inf becomes nil (unknown) and a
// plain float64 holding NaN/Inf becomes 0, which every plain float field in
// the status payload already treats as unknown via omitempty. No value is
// ever invented. encoding/json refuses to marshal NaN/Inf, so without this a
// single bad reading anywhere in the snapshot would blank the whole node.
//
// It copies rather than mutating because the snapshot's slices and pointers
// are shared with the Scheduler's cached reading.
func withoutNonFinite[T any](v T) T {
	out := scrubFloats(reflect.ValueOf(v))
	return out.Interface().(T)
}

func scrubFloats(in reflect.Value) reflect.Value {
	switch in.Kind() {
	case reflect.Float32, reflect.Float64:
		if !isFinite(in.Float()) {
			return reflect.Zero(in.Type())
		}
		return in
	case reflect.Ptr:
		if in.IsNil() {
			return in
		}
		if k := in.Type().Elem().Kind(); (k == reflect.Float32 || k == reflect.Float64) && !isFinite(in.Elem().Float()) {
			return reflect.Zero(in.Type())
		}
		p := reflect.New(in.Type().Elem())
		p.Elem().Set(scrubFloats(in.Elem()))
		return p
	case reflect.Struct:
		out := reflect.New(in.Type()).Elem()
		out.Set(in)
		for i := 0; i < in.NumField(); i++ {
			if out.Field(i).CanSet() {
				out.Field(i).Set(scrubFloats(in.Field(i)))
			}
		}
		return out
	case reflect.Slice:
		if in.IsNil() {
			return in
		}
		out := reflect.MakeSlice(in.Type(), in.Len(), in.Len())
		for i := 0; i < in.Len(); i++ {
			out.Index(i).Set(scrubFloats(in.Index(i)))
		}
		return out
	case reflect.Array:
		out := reflect.New(in.Type()).Elem()
		for i := 0; i < in.Len(); i++ {
			out.Index(i).Set(scrubFloats(in.Index(i)))
		}
		return out
	case reflect.Map:
		if in.IsNil() {
			return in
		}
		out := reflect.MakeMapWithSize(in.Type(), in.Len())
		for _, k := range in.MapKeys() {
			out.SetMapIndex(k, scrubFloats(in.MapIndex(k)))
		}
		return out
	default:
		return in
	}
}
