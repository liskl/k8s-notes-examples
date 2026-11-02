// cnpg-demo is Field Guide 07's one binary: a small web app, a steady writer and a reader, for watching a
// CloudNativePG cluster from the client's side.
//
//	web    [-addr :8080]              GET / says which instance answered: its address, primary or replica,
//	                                  and on the primary the timeline. GET /ro asks the same through
//	                                  PGHOST_RO (the -ro Service). POST /write adds a row, GET /count counts
//	                                  them, GET /healthz answers without Postgres. Every request opens a new
//	                                  connection, so the answers follow the Services as they change.
//	writer [-run R] -every D -for D   one numbered insert every D for D, through every outage: after an error
//	       [-commit M] [-timeout D]   it reconnects and sends the same row again, until Postgres answers. A
//	                                  row is acknowledged when Postgres said yes. At the end it reads the
//	                                  table back and counts the acknowledged rows that are missing. The run's
//	                                  name is -run, or the host name (in a Pod, the Pod's name).
//	reader [-run R] -every D -for D   one line every D through PGHOST_RO: which instance answered, the run's
//	                                  rows and last row there, and how far its replay is behind. Without
//	                                  -run it follows the newest run in the table.
//	version
//
// Connection settings come from libpq's environment variables (PGHOST, PGPORT, PGUSER, PGPASSWORD,
// PGDATABASE, PGSSLMODE, PGSSLROOTCERT, PGSSLCERT, PGSSLKEY), set one by one from the cluster's Secrets;
// never a URI. It never prints a password. Every line it prints fits in 100 characters.
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// version is set at build time: -ldflags "-X main.version=1.0.0".
var version = "dev"

const usage = "usage: cnpg-demo web|writer|reader|version [flags]"

func main() {
	log.SetFlags(log.Ltime)
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "web":
		err = web(ctx, args)
	case "writer":
		err = writer(ctx, args)
	case "reader":
		err = reader(ctx, args)
	case "version":
		fmt.Println("cnpg-demo", version)
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(short(err))
	}
}

// short makes an error one line a capture can print: a Postgres error as its SQLSTATE and message,
// a failed connection as the reason only (pgx starts it with "failed to connect to `user=... database=...`:"),
// anything else as it is; cut to 80 characters.
func short(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return cut(fmt.Sprintf("%s %s", pe.Code, pe.Message), 80)
	}
	s := err.Error()
	var ce *pgconn.ConnectError
	if errors.As(err, &ce) {
		if inner := errors.Unwrap(ce); inner != nil {
			s = "connect: " + inner.Error()
		}
	}
	return cut(strings.ReplaceAll(s, "\n", " "), 80)
}

func cut(s string, n int) string {
	if len(s) > n {
		return s[:n-3] + "..."
	}
	return s
}

// target says where libpq's variables point, without the password.
func target(host string) string {
	if host == "" {
		host = os.Getenv("PGHOST")
	}
	return fmt.Sprintf("host=%s port=%s user=%s db=%s sslmode=%s",
		host, os.Getenv("PGPORT"), os.Getenv("PGUSER"), os.Getenv("PGDATABASE"), os.Getenv("PGSSLMODE"))
}

// connect opens one connection from libpq's variables, to host instead of PGHOST when host is set.
func connect(ctx context.Context, host string, timeout time.Duration) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig("")
	if err != nil {
		return nil, err
	}
	if host != "" {
		cfg.Host = host
	}
	c, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return pgx.ConnectConfig(c, cfg)
}

// whoami is what the web app reports about the instance that answered.
func whoami(ctx context.Context, conn *pgx.Conn) (string, error) {
	var addr string
	var replica bool
	err := conn.QueryRow(ctx, "SELECT host(inet_server_addr()), pg_is_in_recovery()").Scan(&addr, &replica)
	if err != nil {
		return "", err
	}
	if replica {
		return fmt.Sprintf("answered by %s, a replica", addr), nil
	}
	// The timeline is the first 8 hex digits of the current WAL file's name; it goes up by one at every
	// promotion, at once (pg_control_checkpoint() would show it only after the next checkpoint).
	// pg_walfile_name only works on a primary.
	var tli string
	if err := conn.QueryRow(ctx, "SELECT substr(pg_walfile_name(pg_current_wal_lsn()), 1, 8)").Scan(&tli); err != nil {
		return "", err
	}
	n, _ := strconv.ParseInt(tli, 16, 64)
	return fmt.Sprintf("answered by %s, the primary, timeline %d", addr, n), nil
}

func web(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("web", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address")
	fs.Parse(args)
	ro := os.Getenv("PGHOST_RO")

	// handle runs one request on a new connection to host (PGHOST when empty).
	handle := func(host string, f func(context.Context, *pgx.Conn) (string, error)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			c, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			defer cancel()
			conn, err := connect(c, host, 3*time.Second)
			var s string
			if err == nil {
				s, err = f(c, conn)
				conn.Close(context.Background())
			}
			if err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				fmt.Fprintln(w, "error:", short(err))
				return
			}
			fmt.Fprintln(w, s)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", handle("", whoami))
	mux.HandleFunc("GET /ro", func(w http.ResponseWriter, r *http.Request) {
		if ro == "" {
			http.Error(w, "PGHOST_RO is not set", http.StatusNotFound)
			return
		}
		handle(ro, whoami)(w, r)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("POST /write", handle("", func(c context.Context, conn *pgx.Conn) (string, error) {
		if _, err := conn.Exec(c, `CREATE TABLE IF NOT EXISTS visits
			(id bigserial PRIMARY KEY, at timestamptz NOT NULL DEFAULT now(), by inet)`); err != nil {
			return "", err
		}
		var id int64
		var by string
		err := conn.QueryRow(c, `INSERT INTO visits (by) VALUES (inet_server_addr())
			RETURNING id, host(by)`).Scan(&id, &by)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("wrote row %d on %s", id, by), nil
	}))
	mux.HandleFunc("GET /count", handle("", func(c context.Context, conn *pgx.Conn) (string, error) {
		var n int64
		err := conn.QueryRow(c, "SELECT count(*) FROM visits").Scan(&n)
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "42P01" { // no table yet: no rows
			return "rows 0", nil
		}
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("rows %d", n), nil
	}))

	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(c)
	}()
	log.Printf("cnpg-demo %s web on %s", version, *addr)
	log.Printf("rw %s", target(""))
	if ro != "" {
		log.Printf("ro host=%s", ro)
	}
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

const acksTable = `CREATE TABLE IF NOT EXISTS acks
	(run text NOT NULL, id bigint NOT NULL, at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY (run, id))`

// writer inserts rows numbered 1, 2, 3 ... into acks, one every -every, and keeps going through outages.
// After an error (the insert failed or timed out, so it may or may not have committed) it reconnects and
// sends the same row again, as an upsert, until Postgres answers. So a row counts as
// acknowledged only once Postgres said yes to it, and "lost" means one thing: Postgres said yes, and the
// row is not there later. At the end it reads the table back through a fresh connection.
func writer(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("writer", flag.ExitOnError)
	run := fs.String("run", "", "a name for this run, to tell its rows apart (default: the host name)")
	every := fs.Duration("every", 100*time.Millisecond, "time between inserts")
	dur := fs.Duration("for", time.Minute, "how long to write")
	commit := fs.String("commit", "", "synchronous_commit for the session (off, local, on, remote_apply); empty keeps the server's")
	timeout := fs.Duration("timeout", 3*time.Second, "how long one connect or insert may take")
	report := fs.Duration("report", 5*time.Second, "time between progress lines")
	fs.Parse(args)
	if *run == "" {
		h, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("writer: no -run and no host name: %w", err)
		}
		*run = h
	}

	open := func(c context.Context) (*pgx.Conn, error) {
		conn, err := connect(c, "", *timeout)
		if err != nil {
			return nil, err
		}
		cc, cancel := context.WithTimeout(c, *timeout)
		defer cancel()
		if *commit != "" {
			if _, err := conn.Exec(cc, "SELECT set_config('synchronous_commit', $1, false)", *commit); err != nil {
				conn.Close(context.Background())
				return nil, err
			}
		}
		if _, err := conn.Exec(cc, acksTable); err != nil {
			conn.Close(context.Background())
			return nil, err
		}
		return conn, nil
	}

	start := time.Now()
	end := start.Add(*dur)
	var acked []int64
	var conn *pgx.Conn
	var outages, retries int
	var down time.Time        // when the current outage began; zero while writes go through
	var longest time.Duration // the longest outage so far
	lastReport := start
	next := int64(1)
	tick := time.NewTicker(*every)
	defer tick.Stop()
	defer func() {
		if conn != nil {
			conn.Close(context.Background())
		}
	}()
	since := func() string { return fmt.Sprintf("%5.1fs", time.Since(start).Seconds()) }
	failed := func(err error) {
		if down.IsZero() {
			down = time.Now()
			outages++
			log.Printf("%s outage %d at row %d: %s", since(), outages, next, short(err))
		}
	}
	log.Printf("%s writer %s: %s", since(), *run, target(""))
	log.Printf("%s one insert every %s for %s, synchronous_commit %s", since(), *every, *dur, or(*commit, "as the server says"))

	for time.Now().Before(end) && ctx.Err() == nil {
		select {
		case <-ctx.Done():
			continue
		case <-tick.C:
		}
		if conn == nil {
			c, err := open(ctx)
			if err != nil {
				failed(err)
				continue
			}
			conn = c
		}
		// A resend must write too: a commit that timed out waiting for a synchronous standby has already
		// committed locally, and an insert that did nothing would write no WAL, wait for no standby, and
		// so be acknowledged without ever being replicated.
		c, cancel := context.WithTimeout(ctx, *timeout)
		_, err := conn.Exec(c, `INSERT INTO acks (run, id) VALUES ($1, $2)
			ON CONFLICT (run, id) DO UPDATE SET at = excluded.at`, *run, next)
		cancel()
		if err != nil {
			failed(err)
			retries++
			conn.Close(context.Background())
			conn = nil
			continue
		}
		acked = append(acked, next)
		if !down.IsZero() {
			d := time.Since(down)
			longest = max(longest, d)
			log.Printf("%s back after %.1fs: row %d acknowledged", since(), d.Seconds(), next)
			down = time.Time{}
		}
		next++
		if time.Since(lastReport) >= *report {
			lastReport = time.Now()
			log.Printf("%s acknowledged %d", since(), len(acked))
		}
	}
	if !down.IsZero() {
		longest = max(longest, time.Since(down))
	}
	log.Printf("%s done: acknowledged %d, outages %d, retries %d, longest outage %.1fs",
		since(), len(acked), outages, retries, longest.Seconds())

	// The check: read every id of this run back, through a fresh connection (the old one may be on an
	// instance that is no longer the primary). Retry while the cluster settles.
	if conn != nil {
		conn.Close(context.Background())
		conn = nil
	}
	cctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var present map[int64]bool
	var err error
	for {
		var c *pgx.Conn
		if c, err = connect(cctx, "", *timeout); err == nil {
			present, err = readIDs(cctx, c, *run)
			c.Close(context.Background())
			if err == nil {
				break
			}
		}
		if cctx.Err() != nil {
			return fmt.Errorf("check: %s", short(err))
		}
		time.Sleep(time.Second)
	}
	var lost []int64
	for _, id := range acked {
		if !present[id] {
			lost = append(lost, id)
		}
	}
	log.Printf("check: acknowledged %d, present %d, lost %d", len(acked), len(acked)-len(lost), len(lost))
	if len(lost) > 0 {
		log.Printf("check: lost rows %s", spans(lost))
	}
	return nil
}

func or(s, dflt string) string {
	if s == "" {
		return dflt
	}
	return s
}

func readIDs(ctx context.Context, c *pgx.Conn, run string) (map[int64]bool, error) {
	rows, err := c.Query(ctx, "SELECT id FROM acks WHERE run = $1", run)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, err
	}
	m := make(map[int64]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m, nil
}

// spans writes sorted ids as ranges, "41-57, 60", cut to fit one line.
func spans(ids []int64) string {
	var parts []string
	for i := 0; i < len(ids); {
		j := i
		for j+1 < len(ids) && ids[j+1] == ids[j]+1 {
			j++
		}
		if i == j {
			parts = append(parts, strconv.FormatInt(ids[i], 10))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", ids[i], ids[j]))
		}
		i = j + 1
	}
	return cut(strings.Join(parts, ", "), 70)
}

// reader prints, every -every, what the -ro Service shows of a writer's run: which instance answered, how
// many of the run's rows it has and the last one, and how long ago it replayed its last transaction
// (pg_last_xact_replay_timestamp; "primary" when the instance is not in recovery).
func reader(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("reader", flag.ExitOnError)
	run := fs.String("run", "", "the writer's run to follow (default: the newest run in the table)")
	every := fs.Duration("every", time.Second, "time between lines")
	dur := fs.Duration("for", time.Minute, "how long to read")
	fs.Parse(args)
	host := or(os.Getenv("PGHOST_RO"), os.Getenv("PGHOST"))
	start := time.Now()
	since := func() string { return fmt.Sprintf("%5.1fs", time.Since(start).Seconds()) }
	log.Printf("%s reader: %s", since(), target(host))
	following := ""
	tick := time.NewTicker(*every)
	defer tick.Stop()
	for end := start.Add(*dur); time.Now().Before(end) && ctx.Err() == nil; {
		select {
		case <-ctx.Done():
			continue
		case <-tick.C:
		}
		// A new connection each line, so the Service may send each one to another replica.
		conn, err := connect(ctx, host, 3*time.Second)
		if err != nil {
			log.Printf("%s %s", since(), short(err))
			continue
		}
		c, cancel := context.WithTimeout(ctx, 3*time.Second)
		var addr string
		var replica bool
		var rows, last int64
		var delay *float64
		// The run: -run, or the run of the newest row this instance has.
		r := *run
		err = conn.QueryRow(c, `SELECT host(inet_server_addr()), pg_is_in_recovery(),
			extract(epoch FROM now() - pg_last_xact_replay_timestamp())::float8`).Scan(&addr, &replica, &delay)
		if err == nil && r == "" {
			err = conn.QueryRow(c, "SELECT coalesce((SELECT run FROM acks ORDER BY at DESC LIMIT 1), '')").Scan(&r)
		}
		if err == nil && r != following && r != "" {
			following = r
			log.Printf("%s following run %s", since(), r)
		}
		if err == nil {
			err = conn.QueryRow(c, "SELECT count(*), coalesce(max(id), 0) FROM acks WHERE run = $1", r).Scan(&rows, &last)
		}
		cancel()
		conn.Close(context.Background())
		var pe *pgconn.PgError
		switch {
		case errors.As(err, &pe) && pe.Code == "42P01": // the writer has not made the table yet
			log.Printf("%s no table acks yet", since())
		case err != nil:
			log.Printf("%s %s", since(), short(err))
		case !replica:
			log.Printf("%s %s primary: rows %d, last %d", since(), addr, rows, last)
		case delay == nil:
			log.Printf("%s %s replica: rows %d, last %d, nothing replayed yet", since(), addr, rows, last)
		default:
			log.Printf("%s %s replica: rows %d, last %d, last replay %.1fs ago", since(), addr, rows, last, *delay)
		}
	}
	return nil
}
