# envloader

[![Go Reference](https://pkg.go.dev/badge/github.com/tahirub/envloader.svg)](https://pkg.go.dev/github.com/tahirub/envloader)
![Go version](https://img.shields.io/badge/go-1.22%2B-00ADD8)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

Load environment variables and `.env` files into a Go struct. One file, standard library only.

Loading config shouldn't cost you a dependency. envloader is a single file that you can copy into your project and own, or `go get` if you prefer.

- Struct tags for the key name, `required` and `default=`
- The environment wins over `.env`, so a file never overrides a real variable
- Strings, bools, numbers, durations, slices, pointers and any `encoding.TextUnmarshaler` (`time.Time`, `net.IP`, ...)
- Every bad field reported in one error, and no error message ever contains a value, so secrets stay out of your logs
- Pluggable environment and file opener for tests

## Install

Requires **Go 1.22** or later. No dependencies.

### Copy the file

```sh
mkdir -p internal/envloader
curl -fsSL -o internal/envloader/envloader.go \
  https://raw.githubusercontent.com/tahirub/envloader/v0.1.0/envloader.go
```

Import it from your own module, for example `example.com/yourapp/internal/envloader`. If you put it in a folder with another name, change the `package` line to match. Keep the license header at the top of the file.

### Or add it as a module

```sh
go get github.com/tahirub/envloader
```

## Quick start

```go
package main

import (
	"fmt"
	"log"
	"time"

	"github.com/tahirub/envloader"
)

type Config struct {
	Port    int           `env:"PORT,default=8080"`
	Timeout time.Duration `env:"TIMEOUT,default=5s"`
	DSN     string        `env:"DATABASE_URL,required"`
}

func main() {
	var cfg Config
	if err := envloader.Load(&cfg); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%+v\n", cfg)
}
```

```sh
# .env
DATABASE_URL=postgres://localhost:5432/app
```

`Load` reads `.env` from the working directory if it exists, then the process environment. `PORT=9000 go run .` overrides the file and the default.

## A real config package

A typical setup keeps config in its own package, validates values that parse fine but would break the app, and takes the lookup function as a parameter so tests can pass a map instead of touching the real environment.

```go
package config

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tahirub/envloader"
)

type ServerConfig struct {
	Host              string        `env:"SERVER_HOST,default=127.0.0.1"`
	Port              int           `env:"SERVER_PORT,default=8080"`
	ReadHeaderTimeout time.Duration `env:"SERVER_READ_HEADER_TIMEOUT,default=5s"`
	ReadTimeout       time.Duration `env:"SERVER_READ_TIMEOUT,default=15s"`
	WriteTimeout      time.Duration `env:"SERVER_WRITE_TIMEOUT,default=20s"`
	IdleTimeout       time.Duration `env:"SERVER_IDLE_TIMEOUT,default=120s"`
	// should be greater than the longest time a request takes to complete
	ShutdownTimeout time.Duration `env:"SERVER_SHUTDOWN_TIMEOUT,default=25s"`
	AllowedOrigins  []string      `env:"SERVER_ALLOWED_ORIGINS"`
}

type DatabaseConfig struct {
	DSN string `env:"DATABASE_URL,required"`

	MaxConns int32 `env:"DB_MAX_CONNS,default=25"`
	MinConns int32 `env:"DB_MIN_CONNS,default=2"`

	ConnMaxLifetime   time.Duration `env:"DB_CONN_MAX_LIFETIME,default=5m"`
	ConnMaxIdleTime   time.Duration `env:"DB_CONN_MAX_IDLE_TIME,default=30m"`
	HealthCheckPeriod time.Duration `env:"DB_HEALTH_CHECK_PERIOD,default=1m"`
	ConnectTimeout    time.Duration `env:"DB_CONNECT_TIMEOUT,default=5s"`
}

type StorageConfig struct {
	TokenKey        string `env:"R2_TOKEN_KEY"`
	AccountID       string `env:"R2_ACCOUNT_ID"`
	AccessKeyID     string `env:"R2_ACCESS_KEY_ID"`
	SecretAccessKey string `env:"R2_SECRET_ACCESS_KEY"`
	Bucket          string `env:"R2_BUCKET"`
	Endpoint        string `env:"R2_ENDPOINT"`
}

type Config struct {
	Env      string `env:"ENV,default=production"`
	Server   ServerConfig
	Database DatabaseConfig
	Storage  StorageConfig
}

// Load reads the config from lookupEnv first and .env second, then validates it.
func Load(lookupEnv func(string) (string, bool)) (*Config, error) {
	var cfg Config
	loader := envloader.Loader{LookupEnv: lookupEnv}
	if err := loader.Load(&cfg); err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return &cfg, nil
}

// Validate catches values that load fine but would break the app at runtime.
// Messages never include the values themselves, since some are secrets.
func (c *Config) Validate() error {
    ...
}
```

In `main`:

```go
cfg, err := config.Load(os.LookupEnv)
if err != nil {
	log.Fatal(err)
}
```

In a test:

```go
env := map[string]string{"DATABASE_URL": "postgres://localhost/test"}
cfg, err := config.Load(func(key string) (string, bool) {
	v, ok := env[key]
	return v, ok
})
```

## Struct tags

| Tag | Meaning |
| --- | --- |
| `env:"NAME"` | Read `NAME` |
| `env:"NAME,required"` | Fail if `NAME` is unset or empty |
| `env:"NAME,default=VALUE"` | Use `VALUE` if `NAME` is unset or empty |
| `env:"-"` | Skip the field |
| no tag | Read a variable named exactly like the field, such as `Port` |

`default=` must come last and takes the rest of the tag, commas included: `env:"HOSTS,default=a,b"` defaults a slice to `[a b]`. `required` and `default` can't be combined. Unexported fields are ignored.

### Nested structs

Fields of a nested struct are read as if they belonged to the parent, which is why `Server ServerConfig` above needs no tag. Tag the struct field to prefix every key below it:

```go
type CacheConfig struct {
	Addr string `env:"ADDR"`
}

type Config struct {
	Cache CacheConfig `env:"CACHE"` // reads CACHE_ADDR
}
```

Embedded structs never add a prefix. Nil pointers to structs are allocated. `required` and `default` only apply to leaf fields, and two fields that resolve to the same key are an error.

## Supported types

| Type | Accepted values |
| --- | --- |
| `string` | anything |
| `bool` | `1 t T true True TRUE 0 f F false False FALSE` |
| `int*`, `uint*` | Go integer literals: `42`, `0x1F`, `0o17`, `0b101`, `1_000` |
| `float32`, `float64` | `3.14`, `1e-3` |
| `time.Duration` | `250ms`, `30s`, `1h30m` |
| `[]byte` | the raw value |
| `[]T` for any supported `T` | `a, b, c` (items trimmed) |
| `encoding.TextUnmarshaler` | whatever the type accepts, such as RFC 3339 for `time.Time` |
| `*T` for any of the above | allocated only when a value is set |

Named types such as `type Level int` work too. Because integers use Go literal syntax, a leading zero means octal: `010` is `8`.

### Empty values

`KEY=` counts as unset for `required` and `default`, so a line copied from `.env.example` falls back to the default or fails as missing. On a field with neither, the empty value is assigned as is: strings become `""`, slices become empty, and numbers, bools and durations fail to parse.

## Where values come from

```go
envloader.Load(&cfg)                       // environment, then .env if it exists
envloader.Load(&cfg, ".env", ".env.local") // environment, then .env.local, then .env
```

For each key, the environment is checked first, then the files. When two files set the same key, the later file wins. The implicit `.env` may be missing; files you name must exist. Values from files are never written to the process environment.

To control the sources, use a `Loader`:

```go
loader := envloader.Loader{
	LookupEnv:    lookup,  // default os.LookupEnv
	Open:         open,    // default os.Open
	MaxLineBytes: 4 << 20, // default 1 MiB
}
err := loader.Load(&cfg)
```

`Bind` skips files entirely and fills the struct from any lookup function. `Parse` reads `.env` content from an `io.Reader` into a map.

## .env format

```sh
# comments start a line
DATABASE_URL=postgres://localhost:5432/app
GREETING="hello, world"
RAW='no $expansion, no \n escapes'
EMPTY=
```

- One `KEY=VALUE` per line, with whitespace around the key and the value trimmed
- Blank lines and lines starting with `#` are skipped
- Matching single or double quotes are removed; everything inside is kept as written
- When a key repeats, the last line wins

What the format deliberately leaves out:

- inline comments: `KEY=a # b` is the value `a # b`
- escape sequences, `$VAR` expansion and multi-line values
- the `export` prefix: `export KEY=value` is read as the key `export KEY`

## Errors

```text
load config: SERVER_PORT (Server.Port int): invalid value: invalid syntax
DATABASE_URL (Database.DSN string): required value not set
```

- Every failing field is returned at once, joined with `errors.Join`
- Each one is a `*FieldError` (key, field path, type), and each malformed `.env` line is a `*ParseError` (file, line)
- Both wrap a sentinel such as `ErrMissingValue`, `ErrInvalidDuration` or `ErrUnmatchedQuote`, so `errors.Is` and `errors.As` work
- Neither message includes the value or the line, because values are often secrets and error messages end up in logs. The raw text is still on the struct if you need it
- If any field fails, the struct is left as it was

```go
if errors.Is(err, envloader.ErrMissingValue) {
	// at least one required variable is missing
}

var fe *envloader.FieldError
if errors.As(err, &fe) {
	log.Printf("check %s", fe.Key)
}
```

## License

[MIT](LICENSE)
