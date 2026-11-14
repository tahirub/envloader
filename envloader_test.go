package envloader_test

import (
	"errors"
	"io"
	"io/fs"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tahirub/envloader"
)

// lookupFrom turns a map into the lookup func Bind and Loader expect.
func lookupFrom(env map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}
}

func TestParse(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		want     map[string]string
		wantErr  error
		wantLine int // checked only when non-zero
	}{
		// keys and separators
		{name: "simple pair", input: `KEY=value`, want: map[string]string{"KEY": "value"}},
		{name: "surrounding whitespace is trimmed", input: `  KEY  =  value  `, want: map[string]string{"KEY": "value"}},
		{name: "tabs around the separator", input: "KEY\t=\tvalue", want: map[string]string{"KEY": "value"}},
		{name: "lowercase and digits in key", input: `my_key_2=value`, want: map[string]string{"my_key_2": "value"}},
		{name: "export prefix is kept in the key", input: `export KEY=value`, want: map[string]string{"export KEY": "value"}},
		{name: "value containing a separator", input: `KEY=a=b`, want: map[string]string{"KEY": "a=b"}},
		{name: "value starting with a separator", input: `KEY==value`, want: map[string]string{"KEY": "=value"}},
		{name: "hash is not an inline comment", input: `KEY=value#tail`, want: map[string]string{"KEY": "value#tail"}},

		// unquoted values
		{name: "empty value", input: `KEY=`, want: map[string]string{"KEY": ""}},
		{name: "whitespace only value", input: `KEY=   `, want: map[string]string{"KEY": ""}},
		{name: "apostrophe in unquoted value", input: `KEY=it's fine`, want: map[string]string{"KEY": "it's fine"}},
		{name: "trailing double quote without opener", input: `KEY=69"`, want: map[string]string{"KEY": `69"`}},
		{name: "trailing single quote without opener", input: `KEY=69'`, want: map[string]string{"KEY": `69'`}},

		// quoted values
		{name: "double quoted", input: `KEY="69"`, want: map[string]string{"KEY": "69"}},
		{name: "single quoted", input: `KEY='69'`, want: map[string]string{"KEY": "69"}},
		{name: "empty double quotes", input: `KEY=""`, want: map[string]string{"KEY": ""}},
		{name: "empty single quotes", input: `KEY=''`, want: map[string]string{"KEY": ""}},
		{name: "nested double quotes", input: `KEY=""69""`, want: map[string]string{"KEY": `"69"`}},
		{name: "triple double quote", input: `KEY="""`, want: map[string]string{"KEY": `"`}},
		{name: "single quotes inside double quotes", input: `KEY="it's"`, want: map[string]string{"KEY": "it's"}},
		{name: "double quotes inside single quotes", input: `KEY='say "hi"'`, want: map[string]string{"KEY": `say "hi"`}},
		{name: "backtick inside double quotes", input: "KEY=\"`\"", want: map[string]string{"KEY": "`"}},
		{name: "quotes preserve inner whitespace", input: `KEY="  padded  "`, want: map[string]string{"KEY": "  padded  "}},
		{name: "escape sequences are not interpreted", input: `KEY="a\nb"`, want: map[string]string{"KEY": `a\nb`}},
		{name: "separator inside quotes", input: `KEY="a=b"`, want: map[string]string{"KEY": "a=b"}},
		{name: "comment marker inside quotes", input: `KEY="# not a comment"`, want: map[string]string{"KEY": "# not a comment"}},

		// whole files
		{name: "empty input", input: ``, want: map[string]string{}},
		{
			name:  "blank lines and comments are skipped",
			input: "# comment\n\nA=1\n   # indented comment\nB=2\n",
			want:  map[string]string{"A": "1", "B": "2"},
		},
		{name: "a later line wins", input: "A=1\nA=2", want: map[string]string{"A": "2"}},

		// errors
		{name: "missing separator", input: `KEYVALUE`, wantErr: envloader.ErrInvalidKeyValuePair},
		{name: "empty key", input: `=value`, wantErr: envloader.ErrEmptyKey},
		{name: "whitespace only key", input: `   =value`, wantErr: envloader.ErrEmptyKey},
		{name: "unbalanced double quote", input: `KEY="69`, wantErr: envloader.ErrUnmatchedQuote},
		{name: "unbalanced single quote", input: `KEY='69`, wantErr: envloader.ErrUnmatchedQuote},
		{name: "mismatched quote pair", input: `KEY="69'`, wantErr: envloader.ErrUnmatchedQuote},
		{name: "mismatched quote pair reversed", input: `KEY='69"`, wantErr: envloader.ErrUnmatchedQuote},
		{name: "lone double quote", input: `KEY="`, wantErr: envloader.ErrUnmatchedQuote},
		{name: "lone single quote", input: `KEY='`, wantErr: envloader.ErrUnmatchedQuote},
		{name: "error reports its line", input: "A=1\n# fine\nBROKEN\n", wantErr: envloader.ErrInvalidKeyValuePair, wantLine: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := envloader.Parse(strings.NewReader(tt.input))

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Parse(%q) error = %v, want %v", tt.input, err, tt.wantErr)
				}
				if got != nil {
					t.Fatalf("Parse(%q) = %v on error, want nil", tt.input, got)
				}
				if tt.wantLine != 0 {
					var pe *envloader.ParseError
					if !errors.As(err, &pe) {
						t.Fatalf("Parse(%q) error is %T, want *envloader.ParseError", tt.input, err)
					}
					if pe.Line != tt.wantLine {
						t.Fatalf("Parse(%q) error line = %d, want %d", tt.input, pe.Line, tt.wantLine)
					}
				}
				return
			}

			if err != nil {
				t.Fatalf("Parse(%q) returned unexpected error: %v", tt.input, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Parse(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestBindTagNames(t *testing.T) {
	type database struct {
		URL string `env:"URL"`
	}
	type spec struct {
		Named    string   `env:"NAMED"`
		Untagged string   // no tag: the field name is the key
		Trailing string   `env:"TRAILING,"`
		Skipped  string   `env:"-"`
		DB       database `env:"DB"` // a tagged struct prefixes its fields: DB_URL
	}

	env := map[string]string{
		"NAMED":    "a",
		"Untagged": "b",
		"TRAILING": "c",
		"Skipped":  "never read",
		"DB_URL":   "d",
	}

	var got spec
	if err := envloader.Bind(&got, lookupFrom(env)); err != nil {
		t.Fatalf("Bind returned unexpected error: %v", err)
	}

	want := spec{Named: "a", Untagged: "b", Trailing: "c", DB: database{URL: "d"}}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestBindMalformedTags(t *testing.T) {
	tests := []struct {
		name string
		spec any
	}{
		{name: "unknown option", spec: &struct {
			A string `env:"A,requird"`
		}{}},
		{name: "required and default together", spec: &struct {
			A string `env:"A,required,default=x"`
		}{}},
		{name: "option on a nested struct", spec: &struct {
			N struct{ B string } `env:"N,required"`
		}{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := envloader.Bind(tt.spec, lookupFrom(nil))
			if !errors.Is(err, envloader.ErrMalformedTag) {
				t.Fatalf("error = %v, want %v", err, envloader.ErrMalformedTag)
			}
		})
	}
}

func TestBindRequiredAndDefault(t *testing.T) {
	type spec struct {
		Port    int      `env:"PORT,default=8080"`
		DSN     string   `env:"DSN,required"`
		Origins []string `env:"ORIGINS,default=a,b"`
	}

	tests := []struct {
		name    string
		env     map[string]string
		want    spec
		wantErr error
	}{
		{
			name: "defaults fill unset keys",
			env:  map[string]string{"DSN": "postgres://x"},
			want: spec{Port: 8080, DSN: "postgres://x", Origins: []string{"a", "b"}},
		},
		{
			name: "set values win over defaults",
			env:  map[string]string{"DSN": "postgres://x", "PORT": "9000", "ORIGINS": "c"},
			want: spec{Port: 9000, DSN: "postgres://x", Origins: []string{"c"}},
		},
		{
			name: "empty value falls back to the default",
			env:  map[string]string{"DSN": "postgres://x", "PORT": ""},
			want: spec{Port: 8080, DSN: "postgres://x", Origins: []string{"a", "b"}},
		},
		{
			name:    "missing required key",
			env:     map[string]string{},
			wantErr: envloader.ErrMissingValue,
		},
		{
			name:    "empty required key",
			env:     map[string]string{"DSN": ""},
			wantErr: envloader.ErrMissingValue,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got spec
			err := envloader.Bind(&got, lookupFrom(tt.env))
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

// recorder decodes itself through a pointer receiver, like time.Time and net.IP.
type recorder struct{ got string }

func (r *recorder) UnmarshalText(b []byte) error {
	r.got = string(b)
	return nil
}

var errRejected = errors.New("rejected")

// rejecter uses a value receiver, the rarer case Bind must also recognize.
type rejecter struct{}

func (rejecter) UnmarshalText([]byte) error {
	return errRejected
}

func TestBindTextUnmarshaler(t *testing.T) {
	var got struct {
		At      time.Time     `env:"AT"`
		IP      net.IP        `env:"IP"`
		Timeout time.Duration `env:"TIMEOUT"` // an int64, parsed as a duration, not as text
		Custom  recorder      `env:"CUSTOM"`
	}
	env := map[string]string{
		"AT":      "2026-01-02T03:04:05Z",
		"IP":      "127.0.0.1",
		"TIMEOUT": "30s",
		"CUSTOM":  "hello",
	}
	if err := envloader.Bind(&got, lookupFrom(env)); err != nil {
		t.Fatalf("Bind returned unexpected error: %v", err)
	}

	if want := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC); !got.At.Equal(want) {
		t.Errorf("At = %v, want %v", got.At, want)
	}
	if want := net.ParseIP("127.0.0.1"); !got.IP.Equal(want) {
		t.Errorf("IP = %v, want %v", got.IP, want)
	}
	if got.Timeout != 30*time.Second {
		t.Errorf("Timeout = %v, want 30s", got.Timeout)
	}
	if got.Custom.got != "hello" {
		t.Errorf("Custom got %q, want %q", got.Custom.got, "hello")
	}

	// the value receiver must be called too, and its error must come back
	var bad struct {
		R rejecter `env:"R"`
	}
	if err := envloader.Bind(&bad, lookupFrom(map[string]string{"R": "x"})); !errors.Is(err, errRejected) {
		t.Errorf("value receiver: error = %v, want %v", err, errRejected)
	}
}

func TestLoaderLoad(t *testing.T) {
	type spec struct {
		A string `env:"A"`
		B string `env:"B"`
	}
	files := map[string]string{".env": "A=file\nB=file\n"}
	open := func(name string) (io.ReadCloser, error) {
		content, ok := files[name]
		if !ok {
			return nil, fs.ErrNotExist
		}
		return io.NopCloser(strings.NewReader(content)), nil
	}

	t.Run("environment wins over the file", func(t *testing.T) {
		loader := envloader.Loader{LookupEnv: lookupFrom(map[string]string{"A": "env"}), Open: open}
		var got spec
		if err := loader.Load(&got); err != nil {
			t.Fatalf("Load returned unexpected error: %v", err)
		}
		if want := (spec{A: "env", B: "file"}); got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("a missing implicit .env is not an error", func(t *testing.T) {
		loader := envloader.Loader{LookupEnv: lookupFrom(nil), Open: func(string) (io.ReadCloser, error) {
			return nil, fs.ErrNotExist
		}}
		var got spec
		if err := loader.Load(&got); err != nil {
			t.Fatalf("Load returned unexpected error: %v", err)
		}
	})

	t.Run("a path the caller names must exist", func(t *testing.T) {
		loader := envloader.Loader{LookupEnv: lookupFrom(nil), Open: open}
		var got spec
		if err := loader.Load(&got, "missing.env"); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("error = %v, want %v", err, fs.ErrNotExist)
		}
	})
}

// error messages end up in logs, so they must never repeat the raw input
func TestErrorsDoNotEchoValues(t *testing.T) {
	const secret = "s3cr3t-value"

	_, err := envloader.Parse(strings.NewReader(`R2_SECRET_ACCESS_KEY="` + secret))
	if err == nil {
		t.Fatal("expected a parse error for the unmatched quote")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("parse error echoes the line: %v", err)
	}

	var spec struct {
		Port int `env:"PORT"`
	}
	err = envloader.Bind(&spec, func(string) (string, bool) { return secret, true })
	if err == nil {
		t.Fatal("expected a field error for a non-numeric port")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("field error echoes the value: %v", err)
	}
}
