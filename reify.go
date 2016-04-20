package ucfg

import (
	"reflect"
	"regexp"
	"strings"
	"time"
)

func (c *Config) Unpack(to interface{}, options ...Option) error {
	opts := makeOptions(options)

	if c == nil {
		return raiseNil(ErrNilConfig)
	}
	if to == nil {
		return raiseNil(ErrNilValue)
	}

	vTo := reflect.ValueOf(to)
	if to == nil || (vTo.Kind() != reflect.Ptr && vTo.Kind() != reflect.Map) {
		return raisePointerRequired(vTo)
	}

	return reifyInto(opts, vTo, c)
}

func reifyInto(opts options, to reflect.Value, from *Config) Error {
	to = chaseValuePointers(to)

	if to, ok := tryTConfig(to); ok {
		return mergeConfig(to.Addr().Interface().(*Config), from)
	}

	tTo := chaseTypePointers(to.Type())
	switch tTo.Kind() {
	case reflect.Map:
		return reifyMap(opts, to, from)
	case reflect.Struct:
		return reifyStruct(opts, to, from)
	}

	return raiseInvalidTopLevelType(to.Interface())
}

func reifyMap(opts options, to reflect.Value, from *Config) Error {
	if to.Type().Key().Kind() != reflect.String {
		return raiseKeyInvalidTypeUnpack(to.Type(), from)
	}

	if len(from.fields.fields) == 0 {
		return nil
	}

	if to.IsNil() {
		to.Set(reflect.MakeMap(to.Type()))
	}
	for k, value := range from.fields.fields {
		key := reflect.ValueOf(k)

		old := to.MapIndex(key)
		var v reflect.Value
		var err Error

		if !old.IsValid() {
			v, err = reifyValue(fieldOptions{opts: opts}, to.Type().Elem(), value)
		} else {
			v, err = reifyMergeValue(fieldOptions{opts: opts}, old, value)
		}

		if err != nil {
			return err
		}
		to.SetMapIndex(key, v)
	}

	return nil
}

func reifyStruct(opts options, orig reflect.Value, cfg *Config) Error {
	orig = chaseValuePointers(orig)

	to := chaseValuePointers(reflect.New(chaseTypePointers(orig.Type())))
	if orig.Kind() == reflect.Struct { // if orig is has been allocated copy into to
		to.Set(orig)
	}

	numField := to.NumField()
	for i := 0; i < numField; i++ {
		stField := to.Type().Field(i)
		vField := to.Field(i)
		name, tagOpts := parseTags(stField.Tag.Get(opts.tag))

		validators, err := parseValidatorTags(stField.Tag.Get(opts.validatorTag))
		if err != nil {
			return raiseCritical(err, "")
		}

		if tagOpts.squash {
			vField := chaseValue(vField)
			switch vField.Kind() {
			case reflect.Struct, reflect.Map:
				if err := reifyInto(opts, vField, cfg); err != nil {
					return err
				}
			default:
				return raiseInlineNeedsObject(cfg, stField.Name, vField.Type())
			}
		} else {
			name = fieldName(name, stField.Name)
			fopts := fieldOptions{opts: opts, tag: tagOpts, validators: validators}
			if err := reifyGetField(cfg, fopts, name, vField); err != nil {
				return err
			}
		}
	}

	if err := tryValidate(to); err != nil {
		return raiseValidation(cfg.ctx, cfg.metadata, err)
	}

	orig.Set(pointerize(orig.Type(), to.Type(), to))
	return nil
}

func reifyGetField(
	cfg *Config,
	opts fieldOptions,
	name string,
	to reflect.Value,
) Error {
	from, field, err := reifyCfgPath(cfg, opts.opts, name)
	if err != nil {
		return err
	}

	value, ok := from.fields.fields[field]
	if !ok {
		// TODO: handle missing config
		return nil
	}

	v, err := reifyMergeValue(opts, to, value)
	if err != nil {
		return err
	}

	to.Set(v)
	return nil
}

func reifyCfgPath(cfg *Config, opts options, field string) (*Config, string, Error) {
	if opts.pathSep == "" {
		return cfg, field, nil
	}

	path := strings.Split(field, opts.pathSep)
	for len(path) > 1 {
		field = path[0]
		path = path[1:]

		sub, exists := cfg.fields.fields[field]
		if !exists {
			return nil, field, raiseMissing(cfg, field)
		}

		cSub, err := sub.toConfig()
		if err != nil {
			return nil, field, raiseExpectedObject(sub)
		}
		cfg = cSub
	}
	field = path[0]

	return cfg, field, nil
}

func reifyValue(
	opts fieldOptions,
	t reflect.Type,
	val value,
) (reflect.Value, Error) {
	if t.Kind() == reflect.Interface && t.NumMethod() == 0 {
		return reflect.ValueOf(val.reify()), nil
	}

	baseType := chaseTypePointers(t)
	if tConfig.ConvertibleTo(baseType) {
		cfg, err := val.toConfig()
		if err != nil {
			return reflect.Value{}, raiseExpectedObject(val)
		}

		v := reflect.ValueOf(cfg).Convert(reflect.PtrTo(baseType))
		if t == baseType { // copy config
			v = v.Elem()
		} else {
			v = pointerize(t, baseType, v)
		}
		return v, nil
	}

	if baseType.Kind() == reflect.Struct {
		sub, err := val.toConfig()
		if err != nil {
			// try primitive
			if v, check := reifyPrimitive(opts, val, t, baseType); check == nil {
				return v, nil
			}

			return reflect.Value{}, raiseExpectedObject(val)
		}

		newSt := reflect.New(baseType)
		if err := reifyInto(opts.opts, newSt, sub); err != nil {
			return reflect.Value{}, err
		}

		if t.Kind() != reflect.Ptr {
			return newSt.Elem(), nil
		}
		return pointerize(t, baseType, newSt), nil
	}

	switch baseType.Kind() {
	case reflect.Map:
		sub, err := val.toConfig()
		if err != nil {
			return reflect.Value{}, raiseExpectedObject(val)
		}

		if baseType.Key().Kind() != reflect.String {
			return reflect.Value{}, raiseKeyInvalidTypeUnpack(baseType, sub)
		}

		newMap := reflect.MakeMap(baseType)
		if err := reifyInto(opts.opts, newMap, sub); err != nil {
			return reflect.Value{}, err
		}
		return newMap, nil

	case reflect.Slice:
		v, err := reifySlice(opts, baseType, castArr(val))
		if err != nil {
			return reflect.Value{}, err
		}
		return pointerize(t, baseType, v), nil
	}

	return reifyPrimitive(opts, val, t, baseType)
}

func reifyMergeValue(
	opts fieldOptions,
	oldValue reflect.Value, val value,
) (reflect.Value, Error) {
	old := chaseValueInterfaces(oldValue)
	t := old.Type()
	old = chaseValuePointers(old)
	if (old.Kind() == reflect.Ptr || old.Kind() == reflect.Interface) && old.IsNil() {
		return reifyValue(opts, t, val)
	}

	baseType := chaseTypePointers(old.Type())
	if tConfig.ConvertibleTo(baseType) {
		sub, err := val.toConfig()
		if err != nil {
			return reflect.Value{}, raiseExpectedObject(val)
		}

		if t == baseType {
			// no pointer -> return type mismatch
			return reflect.Value{}, raisePointerRequired(oldValue)
		}

		// check if old is nil -> copy reference only
		if old.Kind() == reflect.Ptr && old.IsNil() {
			v := val.reflect().Convert(reflect.PtrTo(baseType))
			return pointerize(t, baseType, v), nil
		}

		// check if old == value
		subOld := chaseValuePointers(old).Addr().Convert(tConfigPtr).Interface().(*Config)
		if sub == subOld {
			return oldValue, nil
		}

		// old != value -> merge value into old
		return oldValue, mergeConfig(subOld, sub)
	}

	switch baseType.Kind() {
	case reflect.Map:
		sub, err := val.toConfig()
		if err != nil {
			return reflect.Value{}, raiseExpectedObject(val)
		}
		return old, reifyMap(opts.opts, old, sub)

	case reflect.Struct:
		sub, err := val.toConfig()
		if err != nil {
			return reflect.Value{}, raiseExpectedObject(val)
		}
		return oldValue, reifyStruct(opts.opts, old, sub)

	case reflect.Array:
		return reifyArray(opts, old, baseType, castArr(val))

	case reflect.Slice:
		return reifySlice(opts, baseType, castArr(val))
	}

	return reifyPrimitive(opts, val, t, baseType)
}

func reifyArray(
	opts fieldOptions,
	to reflect.Value, tTo reflect.Type,
	arr *cfgArray,
) (reflect.Value, Error) {
	if arr.Len() != tTo.Len() {
		return reflect.Value{}, raiseArraySize(tTo, arr)
	}
	return reifyDoArray(opts, to, tTo.Elem(), arr)
}

func reifySlice(
	opts fieldOptions,
	tTo reflect.Type,
	arr *cfgArray,
) (reflect.Value, Error) {
	to := reflect.MakeSlice(tTo, arr.Len(), arr.Len())
	return reifyDoArray(opts, to, tTo.Elem(), arr)
}

func reifyDoArray(
	opts fieldOptions,
	to reflect.Value, elemT reflect.Type,
	arr *cfgArray,
) (reflect.Value, Error) {
	for i, from := range arr.arr {
		v, err := reifyValue(opts, elemT, from)
		if err != nil {
			return reflect.Value{}, err
		}
		to.Index(i).Set(v)
	}

	if err := runValidators(to.Interface(), opts.validators); err != nil {
		raiseValidation(arr.ctx, arr.meta(), err)
	}

	return to, nil
}

func castArr(v value) *cfgArray {
	if arr, ok := v.(*cfgArray); ok {
		return arr
	}

	if v.Len() == 0 {
		return &cfgArray{}
	}

	return &cfgArray{
		arr: []value{v},
	}
}

func reifyPrimitive(
	opts fieldOptions,
	val value,
	t, baseType reflect.Type,
) (reflect.Value, Error) {
	// zero initialize value if val==nil
	if _, ok := val.(*cfgNil); ok {
		return pointerize(t, baseType, reflect.Zero(baseType)), nil
	}

	v, err := doReifyPrimitive(opts, val, baseType)
	if err != nil {
		return v, err
	}

	if err := runValidators(v.Interface(), opts.validators); err != nil {
		return reflect.Value{}, raiseValidation(val.Context(), val.meta(), err)
	}

	if err := tryValidate(v.Interface()); err != nil {
		return reflect.Value{}, raiseValidation(val.Context(), val.meta(), err)
	}

	return pointerize(t, baseType, v), nil
}

func doReifyPrimitive(
	opts fieldOptions,
	val value,
	baseType reflect.Type,
) (reflect.Value, Error) {

	extras := map[reflect.Type]func(fieldOptions, value, reflect.Type) (reflect.Value, Error){
		tDuration: reifyDuration,
		tRegexp:   reifyRegexp,
	}

	// try primitive conversion
	kind := baseType.Kind()
	switch {
	case val.typ() == baseType:
		v := val.reflect()
		return v, nil

	case kind == reflect.String:
		s, err := val.toString()
		if err != nil {
			return reflect.Value{}, raiseConversion(val, err, "string")
		}
		return reflect.ValueOf(s), nil

	case isInt(kind):
		v, err := reifyInt(opts, val, baseType)
		if err != nil {
			return v, err
		}
		return v, nil

	case isUint(kind):
		v, err := reifyUint(opts, val, baseType)
		if err != nil {
			return v, err
		}
		return v, nil

	case extras[baseType] != nil:
		v, err := extras[baseType](opts, val, baseType)
		if err != nil {
			return v, err
		}
		return v, nil

	case val.typ().ConvertibleTo(baseType):
		v := val.reflect().Convert(baseType)
		return v, nil
	}

	return reflect.Value{}, raiseToTypeNotSupported(val, baseType)
}

func reifyDuration(
	opts fieldOptions,
	val value,
	_ reflect.Type,
) (reflect.Value, Error) {
	var d time.Duration
	var err error

	switch v := val.(type) {
	case *cfgInt:
		d = time.Duration(v.i) * time.Second
	case *cfgFloat:
		d = time.Duration(v.f * float64(time.Second))
	case *cfgString:
		d, err = time.ParseDuration(v.s)
	default:
		s, err := val.toString()
		if err != nil {
			return reflect.Value{}, raiseInvalidDuration(val, err)
		}

		d, err = time.ParseDuration(s)
	}

	if err != nil {
		return reflect.Value{}, raiseInvalidDuration(val, err)
	}
	return reflect.ValueOf(d), nil
}

func reifyRegexp(
	opts fieldOptions,
	val value,
	_ reflect.Type,
) (reflect.Value, Error) {
	s, err := val.toString()
	if err != nil {
		return reflect.Value{}, raiseConversion(val, err, "regex")
	}

	r, err := regexp.Compile(s)
	if err != nil {
		return reflect.Value{}, raiseConversion(val, err, "regex")
	}
	return reflect.ValueOf(r).Elem(), nil
}

func reifyInt(
	opts fieldOptions,
	val value,
	t reflect.Type,
) (reflect.Value, Error) {
	i, err := val.toInt()
	if err != nil {
		return reflect.Value{}, raiseConversion(val, err, "int")
	}

	tmp := reflect.Zero(t)
	if tmp.OverflowInt(i) {
		return reflect.Value{}, raiseConversion(val, ErrOverflow, "int")
	}
	return reflect.ValueOf(i).Convert(t), nil
}

func reifyUint(
	opts fieldOptions,
	val value,
	t reflect.Type,
) (reflect.Value, Error) {
	u, err := val.toUint()
	if err != nil {
		return reflect.Value{}, raiseConversion(val, err, "uint")
	}

	tmp := reflect.Zero(t)
	if tmp.OverflowUint(u) {
		return reflect.Value{}, raiseConversion(val, ErrOverflow, "uint")
	}
	return reflect.ValueOf(u).Convert(t), nil
}
