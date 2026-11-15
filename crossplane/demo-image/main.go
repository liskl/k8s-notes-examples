// crossplane-demo is Field Guide 08's one binary: a small web app that says what it was given. A platform
// API hands an app a database, a role and a schema; this app prints which ones it really got.
//
//	web [-addr :8080]   GET / prints five lines: the database, the role and the schema this connection
//	                    has, the server's version, and the rows in its table "visits". POST /write adds a
//	                    row (and makes the table the first time). GET /grants says what the role may do in
//	                    its schema. GET /healthz answers without Postgres. Every request opens a new
//	                    connection, so the answers follow the database as it changes.
//	version
//
// Connection settings come from libpq's environment variables (PGHOST, PGPORT, PGUSER, PGPASSWORD,
// PGDATABASE, PGSSLMODE), set one by one from Secrets; never a URI. DEMO_SCHEMA names the schema the app
// works in: it sets search_path to that schema alone, so a schema it was not given shows at once. It never
// prints a password. Every line it prints fits in 100 characters.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// version is set at build time: -ldflags "-X main.version=1.0.0".
var version = "dev"

const usage = "usage: crossplane-demo web|version [flags]"

func main() {
	log.SetFlags(log.Ltime)
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "web":
		if err := web(ctx, args); err != nil {
			log.Fatal(short(err))
		}
	case "version":
		fmt.Println("crossplane-demo", version)
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
}

// short is an error as one line of at most 100 characters: the SQLSTATE and message when Postgres said
// no, else the error's own text.
func short(err error) string {
	s := err.Error()
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		s = pg.Code + " " + pg.Message
	}
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 100 {
		s = s[:97] + "..."
	}
	return s
}

// schema is DEMO_SCHEMA, or "public" when it is not set.
func schema() string {
	if s := os.Getenv("DEMO_SCHEMA"); s != "" {
		return s
	}
	return "public"
}

// connect opens one connection from the PG* variables and points search_path at the app's schema alone.
func connect(ctx context.Context) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig("")
	if err != nil {
		return nil, err
	}
	cfg.ConnectTimeout = 3 * time.Second
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Exec(ctx, "SET search_path TO "+pgx.Identifier{schema()}.Sanitize()); err != nil {
		conn.Close(ctx)
		return nil, err
	}
	return conn, nil
}

// with runs one request's work on a fresh connection and writes a failure as "error: ...", status 503.
func with(w http.ResponseWriter, r *http.Request, work func(ctx context.Context, c *pgx.Conn) (string, error)) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	conn, err := connect(ctx)
	if err == nil {
		defer conn.Close(context.Background())
		var out string
		if out, err = work(ctx, conn); err == nil {
			fmt.Fprint(w, out)
			return
		}
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	fmt.Fprintf(w, "error: %s\n", short(err))
}

// rows counts the app's table; a table that is not there yet counts as none.
func rows(ctx context.Context, c *pgx.Conn) (string, error) {
	var n int64
	err := c.QueryRow(ctx, "SELECT count(*) FROM visits").Scan(&n)
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "42P01" {
		return "0 (no table yet)", nil
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprint(n), nil
}

func web(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("web", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "address to listen on")
	fs.Parse(args)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		with(w, r, func(ctx context.Context, c *pgx.Conn) (string, error) {
			var db, role, ver string
			var sch *string
			err := c.QueryRow(ctx, "SELECT current_database(), current_user, current_schema(), "+
				"current_setting('server_version')").Scan(&db, &role, &sch, &ver)
			if err != nil {
				return "", err
			}
			// current_schema() is null when search_path names no schema this role can use
			s := "none (" + schema() + " is missing, or this role may not use it)"
			if sch != nil {
				s = *sch
			}
			n, err := rows(ctx, c)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("database  %s\nrole      %s\nschema    %s\nserver    PostgreSQL %s\nrows      %s\n",
				db, role, s, ver, n), nil
		})
	})
	mux.HandleFunc("POST /write", func(w http.ResponseWriter, r *http.Request) {
		with(w, r, func(ctx context.Context, c *pgx.Conn) (string, error) {
			if _, err := c.Exec(ctx, "CREATE TABLE IF NOT EXISTS visits "+
				"(id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, at timestamptz NOT NULL DEFAULT now())"); err != nil {
				return "", err
			}
			var id int64
			if err := c.QueryRow(ctx, "INSERT INTO visits DEFAULT VALUES RETURNING id").Scan(&id); err != nil {
				return "", err
			}
			return fmt.Sprintf("wrote row %d in %s.visits\n", id, schema()), nil
		})
	})
	mux.HandleFunc("GET /grants", func(w http.ResponseWriter, r *http.Request) {
		with(w, r, func(ctx context.Context, c *pgx.Conn) (string, error) {
			var role string
			var exists bool
			if err := c.QueryRow(ctx, "SELECT current_user, EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)",
				schema()).Scan(&role, &exists); err != nil {
				return "", err
			}
			if !exists {
				return fmt.Sprintf("role      %s\nschema    %s (missing)\n", role, schema()), nil
			}
			var usage, create bool
			if err := c.QueryRow(ctx, "SELECT has_schema_privilege($1, 'USAGE'), has_schema_privilege($1, 'CREATE')",
				schema()).Scan(&usage, &create); err != nil {
				return "", err
			}
			return fmt.Sprintf("role      %s\nschema    %s\nusage     %t\ncreate    %t\n", role, schema(), usage, create), nil
		})
	})

	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	log.Printf("crossplane-demo %s listening on %s, schema %s", version, *addr, schema())
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
