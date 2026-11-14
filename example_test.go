package envloader_test

import (
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/tahirub/envloader"
)

func ExampleBind() {
	type Config struct {
		Port    int           `env:"PORT,default=8080"`
		Timeout time.Duration `env:"TIMEOUT,default=5s"`
		Origins []string      `env:"ORIGINS"`
		DSN     string        `env:"DATABASE_URL,required"`
	}

	env := map[string]string{
		"ORIGINS":      "https://a.example, https://b.example",
		"DATABASE_URL": "postgres://localhost/app",
	}

	var cfg Config
	err := envloader.Bind(&cfg, func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(cfg.Port, cfg.Timeout, cfg.Origins, cfg.DSN)
	// Output: 8080 5s [https://a.example https://b.example] postgres://localhost/app
}

// A tagged struct field prefixes every key below it.
func ExampleBind_prefix() {
	type Cache struct {
		Addr string `env:"ADDR"`
	}
	type Config struct {
		Cache Cache `env:"CACHE"`
	}

	var cfg Config
	err := envloader.Bind(&cfg, func(key string) (string, bool) {
		if key == "CACHE_ADDR" {
			return "localhost:6379", true
		}
		return "", false
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(cfg.Cache.Addr)
	// Output: localhost:6379
}

// Every bad field is reported at once, and the values never appear in the
// message.
func ExampleBind_errors() {
	var cfg struct {
		Port int    `env:"PORT"`
		DSN  string `env:"DATABASE_URL,required"`
	}

	err := envloader.Bind(&cfg, func(key string) (string, bool) {
		if key == "PORT" {
			return "eighty", true
		}
		return "", false
	})
	fmt.Println(err)
	fmt.Println(errors.Is(err, envloader.ErrMissingValue))
	// Output:
	// PORT (Port int): invalid value: invalid syntax
	// DATABASE_URL (DSN string): required value not set
	// true
}

func ExampleParse() {
	values, err := envloader.Parse(strings.NewReader(`
# database
DATABASE_URL="postgres://localhost/app"
GREETING='hello, world'
EMPTY=
`))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(values["DATABASE_URL"])
	fmt.Println(values["GREETING"])
	fmt.Printf("%q\n", values["EMPTY"])
	// Output:
	// postgres://localhost/app
	// hello, world
	// ""
}

// The environment wins over the file. Here both are replaced so the example
// runs without touching the real ones.
func ExampleLoader() {
	type Config struct {
		Env  string `env:"ENV,default=production"`
		Port int    `env:"PORT"`
	}

	loader := envloader.Loader{
		LookupEnv: func(key string) (string, bool) {
			if key == "PORT" {
				return "9000", true
			}
			return "", false
		},
		Open: func(name string) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("ENV=development\nPORT=8080\n")), nil
		},
	}

	var cfg Config
	if err := loader.Load(&cfg); err != nil {
		log.Fatal(err)
	}
	fmt.Println(cfg.Env, cfg.Port)
	// Output: development 9000
}
