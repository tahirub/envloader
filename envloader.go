// Copyright (c) 2026 tahir
// SPDX-License-Identifier: MIT
// Source: https://github.com/tahirub/envloader

// Package envloader loads environment variables and .env files into a struct,
// using only the standard library.
//
// The package is one file with no dependencies, so it can be added with
// go get or copied straight into a project.
//
// # Quick start
//
//	type Config struct {
//		Port int    `env:"PORT,default=8080"`
//		DSN  string `env:"DATABASE_URL,required"`
//	}
//
//	var cfg Config
//	if err := envloader.Load(&cfg); err != nil {
//		log.Fatal(err)
//	}
//
// Load reads .env from the working directory if it exists, then the process
// environment. A variable set in the environment always wins over the file.
//
// # Struct tags
//
//	env:"NAME"                 read NAME
//	env:"NAME,required"        fail if NAME is unset or empty
//	env:"NAME,default=VALUE"   use VALUE if NAME is unset or empty
//	env:"-"                    skip the field
//
// A field without a tag is read from a variable named exactly like the field.
// default= must be the last option and takes the rest of the tag, commas
// included, so `env:"HOSTS,default=a,b"` defaults a slice to [a b].
// Unexported fields are ignored.
//
// # Nested structs
//
// Fields of a nested struct are read as if they belonged to the parent. Tag
// the struct field to prefix every key below it with NAME_:
//
//	type Config struct {
//		Server ServerConfig              // ServerConfig.Port `env:"SERVER_PORT"` reads SERVER_PORT
//		Cache  CacheConfig `env:"CACHE"` // CacheConfig.Addr `env:"ADDR"` reads CACHE_ADDR
//	}
//
// Embedded structs never add a prefix. Nil pointers to structs are allocated.
// required and default only apply to leaf fields, and two fields that resolve
// to the same key are an error.
//
// # Supported types
//
//   - string, bool, every int, uint and float type, and named types built on them
//   - time.Duration, parsed by time.ParseDuration ("250ms", "1h30m")
//   - []byte, set to the raw value
//   - slices of any supported type, from a comma-separated list with each item trimmed
//   - any type that implements encoding.TextUnmarshaler, such as time.Time, net.IP and netip.Addr
//   - pointers to any of the above, allocated only when a value is set
//
// bool accepts what strconv.ParseBool accepts. Integers accept Go literal
// syntax (0x1F, 0o17, 0b101, 1_000), so a leading zero means octal: "010" is 8.
//
// # Empty values
//
// KEY= counts as unset for required and default, so a line copied from a
// .env.example falls back to the default or fails as missing. On a field with
// neither, the empty value is assigned as is: strings become "", slices become
// empty, and numbers, bools and durations fail to parse.
//
// # .env files
//
// One KEY=VALUE per line, with whitespace around the key and the value
// trimmed. Blank lines and lines starting with # are skipped. A value wrapped
// in matching single or double quotes loses the quotes and keeps everything
// inside as written. When a key repeats, the last line wins.
//
// The format is deliberately small: no inline comments (KEY=a # b is "a # b"),
// no escape sequences, no $VAR expansion, no multi-line values and no export
// prefix. Values read from a file are never written to the process environment.
//
// # Errors
//
// Bind reports every bad field at once, joined with errors.Join, each as a
// *FieldError. A malformed .env line is a *ParseError. Both wrap one of the
// Err values, so errors.Is and errors.As work. Neither message includes the
// value, since values are often secrets and error messages end up in logs.
package envloader

import (
	"bufio"
	"encoding"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"
)

const (
	defaultMaxLineBytes = 1 << 20
	defaultPath         = ".env"
	tagName             = "env"
	skipTag             = "-"
	prefixSep           = "_"
	listSep             = ","
	optRequired         = "required"
	optDefault          = "default="
)

var (
	ErrInvalidSpec     = errors.New("specification must be a pointer to a struct")
	ErrInvalidPointer  = errors.New("specification must be a non-nil pointer")
	ErrMalformedTag    = errors.New("malformed struct tag")
	ErrDuplicateKey    = errors.New("duplicate key")
	ErrRecursiveType   = errors.New("recursive type")
	ErrUnsupportedType = errors.New("unsupported type")

	ErrInvalidKeyValuePair = errors.New("expected KEY=VALUE")
	ErrEmptyKey            = errors.New("empty key")
	ErrUnmatchedQuote      = errors.New("unmatched quote")

	ErrMissingValue    = errors.New("required value not set")
	ErrInvalidValue    = errors.New("invalid value")
	ErrInvalidBoolean  = errors.New("invalid boolean")
	ErrInvalidDuration = errors.New("invalid duration")
)

var textUnmarshaler = reflect.TypeFor[encoding.TextUnmarshaler]()

var durationType = reflect.TypeFor[time.Duration]()
var byteSliceType = reflect.TypeFor[[]byte]()

// ParseError reports a malformed line in a .env file.
type ParseError struct {
	File string 
	Line int    
	Text string
	Err  error
}

func (e *ParseError) Error() string {
	if e.File == "" {
		return fmt.Sprintf("line %d: %v", e.Line, e.Err)
	}
	return fmt.Sprintf("%s:%d: %v", e.File, e.Line, e.Err)
}

func (e *ParseError) Unwrap() error {
	return e.Err
}

// FieldError reports a field that Bind could not set.
type FieldError struct {
	Key   string       
	Field string      
	Type  reflect.Type
	Value string     
	Err   error     
}

func (e *FieldError) Error() string {
	return fmt.Sprintf("%s (%s %s): %v", e.Key, e.Field, e.Type, e.Err)
}

func (e *FieldError) Unwrap() error {
	return e.Err
}

type fieldInfo struct {
	key   string
	path  []string
	value reflect.Value

	required   bool
	defaultVal string
	hasDefault bool
}

type tagOptions struct {
	name       string
	required   bool
	defaultVal string
	hasDefault bool
}

func (fi fieldInfo) String() string {
	return strings.Join(fi.path, ".")
}

type envPair struct {
	key   string
	value string
}

// Loader reads .env files and the environment into a struct. The zero value
// is ready to use and reads from the real environment and disk; set the
// fields to replace either one, for example in tests.
type Loader struct {
	// MaxLineBytes caps the length of a .env line. Zero or less means 1 MiB.
	MaxLineBytes int

	// LookupEnv is checked before the files. Nil means os.LookupEnv.
	LookupEnv func(key string) (string, bool)

	// Open opens a .env file by path. Nil means os.Open. To report a missing
	// file, return an error that wraps fs.ErrNotExist.
	Open func(name string) (io.ReadCloser, error)
}

// Load fills the struct that spec points to. Each key is looked up in the
// environment first and in the .env files second; when files share a key,
// the later file wins.
//
// With no paths, Load reads .env from the working directory and skips it if
// it does not exist. A path passed explicitly must exist.
func (l Loader) Load(spec any, paths ...string) error {
	fileValues, err := l.readPaths(paths...)
	if err != nil {
		return err
	}

	lookupEnv := l.LookupEnv
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}

	// command line passed or os env have precedence over the file ones
	return Bind(spec, func(k string) (string, bool) {
		if value, ok := lookupEnv(k); ok {
			return value, true
		}
		value, ok := fileValues[k]
		return value, ok
	})
}

// Load fills spec from the process environment and the .env files at paths,
// using a zero Loader. See Loader.Load.
func Load(spec any, paths ...string) error {
	var loader Loader
	return loader.Load(spec, paths...)
}

// Parse reads .env content from r and returns its key/value pairs, without
// touching the environment. A malformed line is returned as a *ParseError.
func Parse(r io.Reader) (map[string]string, error) {
	var loader Loader
	values := make(map[string]string)
	if err := loader.readInto(values, r, ""); err != nil {
		return nil, err
	}
	return values, nil
}

// Bind fills the struct that spec points to, calling lookup with each field's
// key. Use it to load from a source other than the environment and .env, such
// as a map in tests.
//
// Problems with the struct itself, such as a malformed tag or a duplicate key,
// are reported before lookup is called. Bind fills a copy and stores it in
// *spec only if every field succeeds, so a failed Bind leaves *spec as it was.
// The copy is shallow: a struct that *spec already points to is filled in place.
func Bind(spec any, lookup func(key string) (string, bool)) error {
	specValue := reflect.ValueOf(spec)
	if !specValue.IsValid() || specValue.Kind() != reflect.Pointer {
		return ErrInvalidSpec
	}
	if specValue.IsNil() {
		return ErrInvalidPointer
	}
	if specValue.Elem().Kind() != reflect.Struct {
		return ErrInvalidSpec
	}

	// create copy so we don't mutate the original while building meta data
	// once the whole data is loading into the copy we swap with original
	staged := reflect.New(specValue.Elem().Type())
	staged.Elem().Set(specValue.Elem())

	fields, err := gatherInfo(staged.Elem())
	if err != nil {
		return err
	}
	if err := fill(fields, lookup); err != nil {
		return err
	}

	specValue.Elem().Set(staged.Elem())
	return nil
}

func gatherInfo(structValue reflect.Value) ([]fieldInfo, error) {
	visiting := make(map[reflect.Type]bool)
	var fields []fieldInfo
	if err := describe(structValue, nil, "", visiting, &fields); err != nil {
		return nil, err
	}

	seen := make(map[string]string, len(fields))
	for _, f := range fields {
		if previous, ok := seen[f.key]; ok {
			return nil, fmt.Errorf("%w: %s is used by %s and %s", ErrDuplicateKey, f.key, previous, f)
		}
		seen[f.key] = f.String()
	}
	return fields, nil
}

func describe(structValue reflect.Value, path []string, prefix string, visiting map[reflect.Type]bool, out *[]fieldInfo) error {
	structType := structValue.Type()
	// check if the same struct is in one of its own fields
	if visiting[structType] {
		return fmt.Errorf("%w: %s", ErrRecursiveType, structType)
	}
	visiting[structType] = true
	defer delete(visiting, structType)

	for i := range structType.NumField() {
		field := structType.Field(i)
		fieldValue := structValue.Field(i)

		if !field.IsExported() {
			continue
		}

		tag, ok := field.Tag.Lookup(tagName)
		// this is used to catch if we are missing closing quote on the struct tags
		if !ok && strings.Contains(string(field.Tag), tagName+`:"`) {
			return fmt.Errorf("%w on %s.%s: `%s`", ErrMalformedTag, structType, field.Name, field.Tag)
		}

		if tag == skipTag {
			continue
		}

		opts, err := parseTag(tag)
		if err != nil {
			return fmt.Errorf("%s.%s: %w", structType, field.Name, err)
		}
		name := opts.name

		// we track the path joined by "." to give the loc of the field in errors.
		// we keep a copy because appending to path directly would share the
		// backing array, so sibling recursive branches would overwrite each other
		fieldPath := append(append(make([]string, 0, len(path)+1), path...), field.Name)

		elemType := field.Type
		for elemType.Kind() == reflect.Pointer {
			elemType = elemType.Elem()
		}

		// field types like time.Time are handled by their own decode func, so we
		// treat them as leaves and don't walk into them.
		if elemType.Kind() == reflect.Struct && !decodable(elemType) {
			if opts.required || opts.hasDefault {
				return fmt.Errorf("%w on %s.%s: required and default only apply to leaf fields", ErrMalformedTag, structType, field.Name)
			}
			for fieldValue.Kind() == reflect.Pointer {
				if fieldValue.IsNil() {
					if !fieldValue.CanSet() {
						break
					}
					fieldValue.Set(reflect.New(fieldValue.Type().Elem()))
				}
				fieldValue = fieldValue.Elem()
			}

			// a leaf pointer like *int is left alone and allocated in setValue
			if fieldValue.Kind() == reflect.Pointer {
				continue
			}

			// an embedded struct shares the parent name, a named one prefixes
			// every key below it, but only if it is tagged, otherwise
			// `Database DatabaseConfig` would turn DATABASE_URL into DATABASE_DATABASE_URL
			nestedPrefix := prefix
			if name != "" && !field.Anonymous {
				nestedPrefix = prefix + name + prefixSep
			}
			if err := describe(fieldValue, fieldPath, nestedPrefix, visiting, out); err != nil {
				return err
			}
			continue
		}

		if !fieldValue.CanSet() {
			continue
		}
		if name == "" {
			name = field.Name
		}
		*out = append(*out, fieldInfo{
			key:        prefix + name,
			path:       fieldPath,
			value:      fieldValue,
			required:   opts.required,
			defaultVal: opts.defaultVal,
			hasDefault: opts.hasDefault,
		})
	}

	return nil
}

// parseTag reads `env:"NAME"`, `env:"NAME,required"` and `env:"NAME,default=VALUE"`.
// default= must come last and takes the rest of the tag, so a default can hold
// commas, like `default=a,b` for a slice.
func parseTag(tag string) (tagOptions, error) {
	name, rest, _ := strings.Cut(tag, listSep)
	opts := tagOptions{name: name}

	for rest != "" {
		if value, ok := strings.CutPrefix(rest, optDefault); ok {
			opts.defaultVal, opts.hasDefault = value, true
			break
		}

		var opt string
		opt, rest, _ = strings.Cut(rest, listSep)
		if opt != optRequired {
			return tagOptions{}, fmt.Errorf("%w: unknown option %q", ErrMalformedTag, opt)
		}
		opts.required = true
	}

	if opts.required && opts.hasDefault {
		return tagOptions{}, fmt.Errorf("%w: required and default cannot be combined", ErrMalformedTag)
	}
	return opts, nil
}

// decodable reports whether t can decode itself from text. UnmarshalText is
// declared on the pointer receiver `func (t *T) UnmarshalText(...)` so it can
// write to the original value, which means it is only in *T's method set and not
// in T's. so we check *T as well, and T on its own for the rare value receiver
func decodable(t reflect.Type) bool {
	return t.Implements(textUnmarshaler) || reflect.PointerTo(t).Implements(textUnmarshaler)
}

func (l Loader) readPaths(paths ...string) (map[string]string, error) {
	// an implicit .env; missing is not an error, but a
	// path the caller named explicitly has to exist
	optional := len(paths) == 0
	if optional {
		paths = []string{defaultPath}
	}

	open := l.Open
	if open == nil {
		open = func(name string) (io.ReadCloser, error) {
			return os.Open(name)
		}
	}

	values := make(map[string]string)
	for _, p := range paths {
		fd, err := open(p)
		if err != nil {
			if optional && errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		err = l.readInto(values, fd, p)

		fd.Close()
		if err != nil {
			return nil, err
		}
	}

	return values, nil
}

func (l Loader) readInto(values map[string]string, r io.Reader, file string) error {
	maxLineBytes := l.MaxLineBytes
	if maxLineBytes <= 0 {
		maxLineBytes = defaultMaxLineBytes
	}

	sc := bufio.NewScanner(r)
	sc.Split(bufio.ScanLines)
	sc.Buffer(make([]byte, 0, min(maxLineBytes, bufio.MaxScanTokenSize)), maxLineBytes)

	for lineNo := 1; sc.Scan(); lineNo++ {
		line := sc.Text()

		ln := strings.TrimSpace(line)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}

		kv, err := parseLine(ln)
		if err != nil {
			return &ParseError{File: file, Line: lineNo, Text: line, Err: err}
		}
		values[kv.key] = kv.value
	}

	if err := sc.Err(); err != nil {
		return fmt.Errorf("read %s: %w", file, err)
	}

	return nil
}

func parseLine(line string) (envPair, error) {
	before, after, found := strings.Cut(line, "=")
	if !found {
		return envPair{}, ErrInvalidKeyValuePair
	}

	key := strings.TrimSpace(before)
	if key == "" {
		return envPair{}, ErrEmptyKey
	}

	value, err := removeQuotes(strings.TrimSpace(after))
	if err != nil {
		return envPair{}, err
	}

	return envPair{key: key, value: value}, nil
}

func removeQuotes(str string) (string, error) {
	if str == "" {
		return "", nil
	}
	quote := str[0]
	if quote != '\'' && quote != '"' {
		return str, nil
	}

	strLen := len(str)
	if strLen < 2 || str[strLen-1] != quote {
		return "", fmt.Errorf("%w: %c", ErrUnmatchedQuote, quote)
	}
	return str[1 : strLen-1], nil
}

func fill(fields []fieldInfo, lookupFunc func(string) (string, bool)) error {
	var errs []error
	for _, f := range fields {
		value, ok := lookupFunc(f.key)

		// an empty value counts as unset for these options: a `KEY=` line copied
		// from .env.example should fall back to the default or fail as required
		if !ok || value == "" {
			switch {
			case f.hasDefault:
				value = f.defaultVal
			case f.required:
				errs = append(errs, &FieldError{
					Key:   f.key,
					Field: f.String(),
					Type:  f.value.Type(),
					Err:   ErrMissingValue,
				})
				continue
			case !ok:
				continue
			}
		}

		if err := setValue(f.value, value); err != nil {
			errs = append(errs, &FieldError{
				Key:   f.key,
				Field: f.String(),
				Type:  f.value.Type(),
				Value: value,
				Err:   err,
			})
		}
	}
	return errors.Join(errs...)
}

func setValue(field reflect.Value, raw string) error {
	// we allocate here and not while walking the struct, so a pointer field that
	// nobody sets stays nil. the loop also covers more than one level, like **T
	for field.Kind() == reflect.Pointer {
		if field.IsNil() {
			field.Set(reflect.New(field.Type().Elem()))
		}
		field = field.Elem()
	}

	if field.CanAddr() && decodable(field.Type()) {
		if decoder, ok := field.Addr().Interface().(encoding.TextUnmarshaler); ok {
			return decoder.UnmarshalText([]byte(raw))
		}
	}

	switch field.Kind() {

	case reflect.String:
		field.SetString(raw)

	case reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("%w (want 1, t, T, true, True, TRUE, 0, f, F, false, False or FALSE)", ErrInvalidBoolean)
		}
		field.SetBool(b)

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		// as time.Duration is an int64, ParseInt would fail if we pass 30s
		if field.Type() == durationType {
			d, err := time.ParseDuration(raw)
			if err != nil {
				return fmt.Errorf("%w (want a unit suffixed duration such as 250ms, 30s or 1h30m)", ErrInvalidDuration)
			}
			field.SetInt(int64(d))
			return nil
		}
		parsed, err := strconv.ParseInt(raw, 0, field.Type().Bits())
		if err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidValue, numReason(err))
		}
		field.SetInt(parsed)

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		parsed, err := strconv.ParseUint(raw, 0, field.Type().Bits())
		if err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidValue, numReason(err))
		}
		field.SetUint(parsed)

	case reflect.Float32, reflect.Float64:
		parsed, err := strconv.ParseFloat(raw, field.Type().Bits())
		if err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidValue, numReason(err))
		}
		field.SetFloat(parsed)

	case reflect.Slice:
		return setSlice(field, raw)

	default:
		return fmt.Errorf("%w: %s", ErrUnsupportedType, field.Type())
	}

	return nil
}

// strconv wraps the cause in a NumError whose message repeats the input, and the
// input can be a secret. unwrap to the bare reason so the value never reaches the
// message and errors.Is(err, strconv.ErrRange) still works
func numReason(err error) error {
	// errors.As keeps this file building on Go 1.22. on Go 1.26+ it can be:
	//
	//	if numErr, ok := errors.AsType[*strconv.NumError](err); ok {
	//		return numErr.Err
	//	}
	var numErr *strconv.NumError
	if errors.As(err, &numErr) {
		return numErr.Err
	}
	return err
}

func setSlice(target reflect.Value, raw string) error {
	if target.Type() == byteSliceType {
		target.SetBytes([]byte(raw))
		return nil
	}
	if raw == "" {
		target.Set(reflect.MakeSlice(target.Type(), 0, 0))
		return nil
	}

	parts := strings.Split(raw, listSep)
	s := reflect.MakeSlice(target.Type(), len(parts), len(parts))
	for i, v := range parts {
		if err := setValue(s.Index(i), strings.TrimSpace(v)); err != nil {
			return fmt.Errorf("element %d: %w", i, err)
		}
	}
	target.Set(s)
	return nil
}
