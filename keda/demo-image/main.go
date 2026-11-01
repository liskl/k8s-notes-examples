// keda-demo is Field Guide 06's one binary: small producers and slow workers for the event sources the book
// scales on, plus the servers KEDA's metrics-api and external scalers talk to.
//
//	redis-push   -list L -n N          LPUSH N jobs onto a Redis list
//	redis-work   -list L               take jobs from the list (BRPOP), one at a time
//	stream-push  -stream S -group G -n N   XADD N jobs (and make the consumer group, so lag counts from zero)
//	stream-work  -stream S -group G    read the group (XREADGROUP), XACK when done; picks up abandoned entries
//	rabbit-push  -queue Q -n N         publish N persistent jobs (-every D -for D: one at a time)
//	rabbit-work  -queue Q              consume with prefetch 1, ack when done
//	kafka-push   -topic T -n N         produce N keyed records (spread over the partitions)
//	kafka-work   -topic T -group G     consume in a group, commit when done
//	pg-push      -n N                  insert N 'todo' rows into the table tasks (made if missing)
//	pg-work                            take a 'todo' row (FOR UPDATE SKIP LOCKED), mark it 'done'
//	job          -list L               a ScaledJob's worker: take ONE job from a Redis list, do it, exit
//	value-server [-audience A]         GET /value is {"backlog":N}, POST /value?set=N sets it; with -audience
//	                                   every GET must carry a bearer token the API server accepts for A
//	scaler                             a KEDA external scaler (gRPC :9090) on a value set over HTTP (:8080)
//	burn-server                        /burn?ms=N spends N ms of CPU, /hold?mb=N&for=D holds memory
//	load         -url U -c C -for D    C clients GET U until D is up, then one summary line
//	serve        [-startup-delay D]    a small web app that waits D before it listens (a slow cold start)
//
// Workers spend -each on a job. On SIGTERM they finish the job in hand (-on-term finish), walk away from it
// (-on-term drop), or carry on as if nothing came (-on-term ignore, until the kubelet's SIGKILL), so a page
// can show what a scale-down does to work in flight. Connection settings come from flags or the environment
// (REDIS_ADDR, REDIS_PASSWORD, RABBITMQ_URL, KAFKA_BROKERS, and libpq's PG* variables).
// It never prints a password or a token.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/grpc"

	es "github.com/liskl/k8s-notes-examples/keda/demo-image/externalscaler"
)

// version is set at build time: -ldflags "-X main.version=1.0.0".
var version = "dev"

const usage = "usage: keda-demo redis-push|redis-work|stream-push|stream-work|rabbit-push|rabbit-work|" +
	"kafka-push|kafka-work|pg-push|pg-work|job|value-server|scaler|burn-server|load|serve|version [flags]"

func main() {
	log.SetFlags(log.Ltime)
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "redis-push":
		err = redisPush(ctx, args)
	case "redis-work":
		err = redisWork(ctx, args)
	case "stream-push":
		err = streamPush(ctx, args)
	case "stream-work":
		err = streamWork(ctx, args)
	case "rabbit-push":
		err = rabbitPush(ctx, args)
	case "rabbit-work":
		err = rabbitWork(ctx, args)
	case "kafka-push":
		err = kafkaPush(ctx, args)
	case "kafka-work":
		err = kafkaWork(ctx, args)
	case "pg-push":
		err = pgPush(ctx, args)
	case "pg-work":
		err = pgWork(ctx, args)
	case "job":
		err = oneJob(ctx, args)
	case "value-server":
		err = valueServer(ctx, args)
	case "scaler":
		err = scalerServer(ctx, args)
	case "burn-server":
		err = burnServer(ctx, args)
	case "load":
		err = load(ctx, args)
	case "serve":
		err = serveApp(ctx, args)
	case "version":
		fmt.Println("keda-demo", version)
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		log.Printf("error: %v", err)
		os.Exit(1)
	}
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func host() string {
	h, _ := os.Hostname()
	return h
}

func jobName(i int) string { return fmt.Sprintf("job-%04d", i) }

// job is one unit of work in hand: ack tells the source it is done.
type job struct {
	name string
	ack  func(context.Context) error
}

// workFlags are the flags every worker shares.
type workFlags struct {
	each   time.Duration
	onTerm string
}

func (w *workFlags) add(fs *flag.FlagSet) {
	fs.DurationVar(&w.each, "each", 2*time.Second, "time spent on each job")
	fs.StringVar(&w.onTerm, "on-term", "finish", "on SIGTERM: finish the job in hand, drop it, or ignore it")
}

// work takes jobs until SIGTERM. take returns nil, nil when nothing came in time, so the loop looks at the
// signal again.
func work(ctx context.Context, w workFlags, take func(context.Context) (*job, error)) error {
	if w.onTerm != "finish" && w.onTerm != "drop" && w.onTerm != "ignore" {
		return fmt.Errorf("-on-term must be finish, drop or ignore, not %q", w.onTerm)
	}
	log.Printf("worker %s up: %s a job, on SIGTERM %s", host(), w.each, w.onTerm)
	if w.onTerm == "ignore" {
		term := ctx
		go func() {
			<-term.Done()
			log.Printf("SIGTERM ignored (-on-term ignore): still working until the kubelet's SIGKILL")
		}()
		ctx = context.WithoutCancel(ctx)
	}
	done := 0
	for {
		if ctx.Err() != nil {
			log.Printf("SIGTERM: no job in hand, bye after %d done", done)
			return nil
		}
		j, err := take(ctx)
		if err != nil {
			if ctx.Err() != nil {
				continue
			}
			log.Printf("source error: %v", err)
			sleepCtx(ctx, 2*time.Second)
			continue
		}
		if j == nil {
			continue
		}
		log.Printf("took %s", j.name)
		start := time.Now()
		select {
		case <-time.After(w.each):
		case <-ctx.Done():
			if w.onTerm == "drop" {
				log.Printf("SIGTERM: dropped %s half done, bye after %d done", j.name, done)
				return nil
			}
			log.Printf("SIGTERM: finishing %s first", j.name)
			time.Sleep(w.each - time.Since(start))
		}
		if j.ack != nil {
			actx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := j.ack(actx)
			cancel()
			if err != nil {
				log.Printf("ack %s: %v", j.name, err)
				continue
			}
		}
		done++
		log.Printf("done %s (%d done)", j.name, done)
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}

// Redis

func redisClient(fs *flag.FlagSet, args []string) (*redis.Client, error) {
	addr := fs.String("addr", envOr("REDIS_ADDR", "redis.redis:6379"), "Redis host:port")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return redis.NewClient(&redis.Options{Addr: *addr, Password: os.Getenv("REDIS_PASSWORD")}), nil
}

func redisPush(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("redis-push", flag.ExitOnError)
	list := fs.String("list", "jobs", "list name")
	n := fs.Int("n", 10, "jobs to push")
	rdb, err := redisClient(fs, args)
	if err != nil {
		return err
	}
	defer rdb.Close()
	if *n < 1 {
		return errors.New("-n must be 1 or more")
	}
	start, err := rdb.Incr(ctx, *list+":seq").Result()
	if err != nil {
		return err
	}
	if _, err := rdb.IncrBy(ctx, *list+":seq", int64(*n-1)).Result(); err != nil {
		return err
	}
	vals := make([]any, *n)
	for i := range vals {
		vals[i] = jobName(int(start) + i)
	}
	length, err := rdb.LPush(ctx, *list, vals...).Result()
	if err != nil {
		return err
	}
	log.Printf("pushed %d jobs onto %s, length now %d", *n, *list, length)
	return nil
}

func redisWork(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("redis-work", flag.ExitOnError)
	list := fs.String("list", "jobs", "list name")
	var w workFlags
	w.add(fs)
	rdb, err := redisClient(fs, args)
	if err != nil {
		return err
	}
	defer rdb.Close()
	return work(ctx, w, func(ctx context.Context) (*job, error) {
		v, err := rdb.BRPop(ctx, 2*time.Second, *list).Result()
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		// BRPOP already removed it: a dropped list job is gone for good
		return &job{name: v[1]}, nil
	})
}

func streamPush(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("stream-push", flag.ExitOnError)
	stream := fs.String("stream", "orders", "stream name")
	group := fs.String("group", "workers", "consumer group to make if missing (empty: none)")
	n := fs.Int("n", 10, "jobs to add")
	rdb, err := redisClient(fs, args)
	if err != nil {
		return err
	}
	defer rdb.Close()
	if *group != "" {
		err := rdb.XGroupCreateMkStream(ctx, *stream, *group, "0").Err()
		if err != nil && !strings.HasPrefix(err.Error(), "BUSYGROUP") {
			return err
		}
	}
	start, err := rdb.IncrBy(ctx, *stream+":seq", int64(*n)).Result()
	if err != nil {
		return err
	}
	for i := 0; i < *n; i++ {
		name := jobName(int(start) - *n + 1 + i)
		args := &redis.XAddArgs{Stream: *stream, Values: map[string]any{"job": name}}
		if err := rdb.XAdd(ctx, args).Err(); err != nil {
			return err
		}
	}
	length, _ := rdb.XLen(ctx, *stream).Result()
	log.Printf("added %d jobs to stream %s, length now %d", *n, *stream, length)
	return nil
}

func streamWork(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("stream-work", flag.ExitOnError)
	stream := fs.String("stream", "orders", "stream name")
	group := fs.String("group", "workers", "consumer group")
	claim := fs.Duration("claim-idle", 20*time.Second, "take over entries left pending this long by another")
	var w workFlags
	w.add(fs)
	rdb, err := redisClient(fs, args)
	if err != nil {
		return err
	}
	defer rdb.Close()
	me := host()
	err = rdb.XGroupCreateMkStream(ctx, *stream, *group, "0").Err()
	if err != nil && !strings.HasPrefix(err.Error(), "BUSYGROUP") {
		return err
	}
	toJob := func(m redis.XMessage, how string) *job {
		name, _ := m.Values["job"].(string)
		return &job{name: name + how, ack: func(ctx context.Context) error {
			return rdb.XAck(ctx, *stream, *group, m.ID).Err()
		}}
	}
	return work(ctx, w, func(ctx context.Context) (*job, error) {
		msgs, _, err := rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{Stream: *stream, Group: *group, Consumer: me,
			MinIdle: *claim, Start: "0-0", Count: 1}).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			return nil, err
		}
		if len(msgs) > 0 {
			return toJob(msgs[0], " (reclaimed)"), nil
		}
		res, err := rdb.XReadGroup(ctx, &redis.XReadGroupArgs{Group: *group, Consumer: me,
			Streams: []string{*stream, ">"}, Count: 1, Block: 2 * time.Second}).Result()
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if len(res) == 0 || len(res[0].Messages) == 0 {
			return nil, nil
		}
		return toJob(res[0].Messages[0], ""), nil
	})
}

// oneJob is a ScaledJob's worker: one job, then exit.
func oneJob(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("job", flag.ExitOnError)
	list := fs.String("list", "jobs", "list name")
	each := fs.Duration("each", 10*time.Second, "time spent on the job")
	fail := fs.Bool("fail", false, "exit 1 after the job")
	hang := fs.Bool("hang", false, "never finish")
	rdb, err := redisClient(fs, args)
	if err != nil {
		return err
	}
	defer rdb.Close()
	v, err := rdb.BRPop(ctx, 5*time.Second, *list).Result()
	if errors.Is(err, redis.Nil) {
		log.Printf("job %s: no job waiting, exit", host())
		return nil
	}
	if err != nil {
		return err
	}
	log.Printf("job %s took %s", host(), v[1])
	if *hang {
		log.Printf("hanging on %s (-hang)", v[1])
		<-ctx.Done()
		return errors.New("stopped while hanging")
	}
	sleepCtx(ctx, *each)
	if *fail {
		return fmt.Errorf("%s failed (-fail)", v[1])
	}
	log.Printf("done %s, exit", v[1])
	return nil
}

// RabbitMQ

func rabbitDial(fs *flag.FlagSet, args []string) (*amqp.Connection, *amqp.Channel, string, error) {
	url := fs.String("url", envOr("RABBITMQ_URL", "amqp://rabbitmq.rabbitmq:5672/"), "AMQP URL")
	queue := fs.String("queue", "tasks", "queue name")
	if err := fs.Parse(args); err != nil {
		return nil, nil, "", err
	}
	conn, err := amqp.Dial(*url)
	if err != nil {
		return nil, nil, "", err
	}
	ch, err := conn.Channel()
	if err != nil {
		conn.Close()
		return nil, nil, "", err
	}
	if _, err := ch.QueueDeclare(*queue, true, false, false, false, nil); err != nil {
		conn.Close()
		return nil, nil, "", err
	}
	return conn, ch, *queue, nil
}

func rabbitPush(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("rabbit-push", flag.ExitOnError)
	n := fs.Int("n", 10, "jobs to publish at once")
	every := fs.Duration("every", 0, "with -for: publish one job this often")
	dur := fs.Duration("for", 0, "keep publishing one job every -every for this long")
	conn, ch, queue, err := rabbitDial(fs, args)
	if err != nil {
		return err
	}
	defer conn.Close()
	// publisher confirms: each publish returns once the broker has the job, so the count below is true
	if err := ch.Confirm(false); err != nil {
		return err
	}
	pub := func(i int) error {
		c, err := ch.PublishWithDeferredConfirmWithContext(ctx, "", queue, false, false, amqp.Publishing{
			DeliveryMode: amqp.Persistent, ContentType: "text/plain", Body: []byte(jobName(i))})
		if err != nil {
			return err
		}
		if ok, err := c.WaitContext(ctx); err != nil || !ok {
			return fmt.Errorf("broker did not confirm %s: %v", jobName(i), err)
		}
		return nil
	}
	sent := 0
	if *dur > 0 && *every > 0 {
		t := time.NewTicker(*every)
		defer t.Stop()
		end := time.After(*dur)
		for {
			select {
			case <-ctx.Done():
				log.Printf("published %d jobs to %s, stopped", sent, queue)
				return nil
			case <-end:
				log.Printf("published %d jobs to %s, one every %s for %s", sent, queue, *every, *dur)
				return nil
			case <-t.C:
				sent++
				if err := pub(sent); err != nil {
					return err
				}
			}
		}
	}
	for i := 1; i <= *n; i++ {
		if err := pub(i); err != nil {
			return err
		}
	}
	q, err := ch.QueueDeclarePassive(queue, true, false, false, false, nil)
	if err != nil {
		return err
	}
	log.Printf("published %d jobs to %s, ready now %d", *n, queue, q.Messages)
	return nil
}

func rabbitWork(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("rabbit-work", flag.ExitOnError)
	var w workFlags
	w.add(fs)
	conn, ch, queue, err := rabbitDial(fs, args)
	if err != nil {
		return err
	}
	// closing the connection hands any unacked job back to the queue
	defer conn.Close()
	if err := ch.Qos(1, 0, false); err != nil {
		return err
	}
	deliveries, err := ch.Consume(queue, host(), false, false, false, false, nil)
	if err != nil {
		return err
	}
	return work(ctx, w, func(ctx context.Context) (*job, error) {
		select {
		case d, ok := <-deliveries:
			if !ok {
				return nil, errors.New("delivery channel closed")
			}
			return &job{name: string(d.Body), ack: func(context.Context) error { return d.Ack(false) }}, nil
		case <-time.After(2 * time.Second):
			return nil, nil
		case <-ctx.Done():
			return nil, nil
		}
	})
}

// Kafka

func kafkaBrokers(s string) []string { return strings.Split(s, ",") }

func kafkaPush(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("kafka-push", flag.ExitOnError)
	brokers := fs.String("brokers", envOr("KAFKA_BROKERS", "kafka.kafka:9092"), "bootstrap brokers")
	topic := fs.String("topic", "events", "topic")
	n := fs.Int("n", 10, "records to produce")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cl, err := kgo.NewClient(kgo.SeedBrokers(kafkaBrokers(*brokers)...), kgo.DefaultProduceTopic(*topic))
	if err != nil {
		return err
	}
	defer cl.Close()
	recs := make([]*kgo.Record, *n)
	stamp := time.Now().UnixNano()
	for i := range recs {
		name := jobName(i + 1)
		recs[i] = &kgo.Record{Key: []byte(fmt.Sprintf("%d-%d", stamp, i)), Value: []byte(name)}
	}
	if err := cl.ProduceSync(ctx, recs...).FirstErr(); err != nil {
		return err
	}
	parts := map[int32]int{}
	for _, r := range recs {
		parts[r.Partition]++
	}
	keys := make([]int, 0, len(parts))
	for p := range parts {
		keys = append(keys, int(p))
	}
	sort.Ints(keys)
	var spread []string
	for _, p := range keys {
		spread = append(spread, fmt.Sprintf("p%d:%d", p, parts[int32(p)]))
	}
	log.Printf("produced %d records to %s (%s)", *n, *topic, strings.Join(spread, " "))
	return nil
}

func kafkaWork(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("kafka-work", flag.ExitOnError)
	brokers := fs.String("brokers", envOr("KAFKA_BROKERS", "kafka.kafka:9092"), "bootstrap brokers")
	topic := fs.String("topic", "events", "topic")
	group := fs.String("group", "workers", "consumer group")
	var w workFlags
	w.add(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cl, err := kgo.NewClient(kgo.SeedBrokers(kafkaBrokers(*brokers)...), kgo.ConsumerGroup(*group),
		kgo.ConsumeTopics(*topic), kgo.DisableAutoCommit(), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.OnPartitionsAssigned(func(_ context.Context, _ *kgo.Client, m map[string][]int32) {
			log.Printf("assigned partitions %v", m[*topic])
		}))
	if err != nil {
		return err
	}
	// leaving the group hands uncommitted records to the others
	defer cl.Close()
	var buf []*kgo.Record
	return work(ctx, w, func(ctx context.Context) (*job, error) {
		if len(buf) == 0 {
			pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			fetches := cl.PollRecords(pctx, 1)
			cancel()
			if fetches.IsClientClosed() {
				return nil, errors.New("client closed")
			}
			for _, fe := range fetches.Errors() {
				if !errors.Is(fe.Err, context.DeadlineExceeded) && !errors.Is(fe.Err, context.Canceled) {
					return nil, fe.Err
				}
			}
			buf = fetches.Records()
			if len(buf) == 0 {
				return nil, nil
			}
		}
		r := buf[0]
		buf = buf[1:]
		name := fmt.Sprintf("%s (p%d@%d)", r.Value, r.Partition, r.Offset)
		return &job{name: name, ack: func(ctx context.Context) error { return cl.CommitRecords(ctx, r) }}, nil
	})
}

// PostgreSQL

const pgTable = `CREATE TABLE IF NOT EXISTS tasks (
  id serial PRIMARY KEY, name text NOT NULL, state text NOT NULL DEFAULT 'todo',
  worker text, done_at timestamptz)`

func pgConnect(ctx context.Context, fs *flag.FlagSet, args []string) (*pgx.Conn, error) {
	dsn := fs.String("dsn", "", "connection string (empty: libpq's PGHOST, PGUSER, PGPASSWORD, PGDATABASE)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	conn, err := pgx.Connect(ctx, *dsn)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Exec(ctx, pgTable); err != nil {
		conn.Close(ctx)
		return nil, err
	}
	return conn, nil
}

func pgPush(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pg-push", flag.ExitOnError)
	n := fs.Int("n", 10, "rows to insert")
	conn, err := pgConnect(ctx, fs, args)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	_, err = conn.Exec(ctx, `INSERT INTO tasks (name)
	  SELECT 'job-' || lpad((coalesce((SELECT max(id) FROM tasks), 0) + g)::text, 4, '0')
	  FROM generate_series(1, $1) g`, *n)
	if err != nil {
		return err
	}
	var todo int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE state = 'todo'`).Scan(&todo); err != nil {
		return err
	}
	log.Printf("inserted %d tasks, todo now %d", *n, todo)
	return nil
}

func pgWork(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pg-work", flag.ExitOnError)
	var w workFlags
	w.add(fs)
	conn, err := pgConnect(ctx, fs, args)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	me := host()
	// The row stays locked in an open transaction while the job runs: a worker that dies rolls back, and the
	// row is 'todo' again for the next one.
	return work(ctx, w, func(ctx context.Context) (*job, error) {
		tx, err := conn.Begin(ctx)
		if err != nil {
			return nil, err
		}
		var id int
		var name string
		err = tx.QueryRow(ctx, `SELECT id, name FROM tasks WHERE state = 'todo' ORDER BY id
		  LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&id, &name)
		if errors.Is(err, pgx.ErrNoRows) {
			tx.Rollback(context.Background())
			sleepCtx(ctx, time.Second)
			return nil, nil
		}
		if err != nil {
			tx.Rollback(context.Background())
			return nil, err
		}
		return &job{name: name, ack: func(ctx context.Context) error {
			if _, err := tx.Exec(ctx, `UPDATE tasks SET state = 'done', worker = $1, done_at = now() WHERE id = $2`,
				me, id); err != nil {
				tx.Rollback(context.Background())
				return err
			}
			return tx.Commit(ctx)
		}}, nil
	})
}

// The settable value behind value-server and scaler.

type value struct {
	mu      sync.Mutex
	n       float64
	target  float64
	changed chan struct{}
}

func newValue(n, target float64) *value {
	return &value{n: n, target: target, changed: make(chan struct{})}
}

func (v *value) get() (float64, float64, <-chan struct{}) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.n, v.target, v.changed
}

func (v *value) set(n, target *float64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if n != nil {
		v.n = *n
	}
	if target != nil {
		v.target = *target
	}
	close(v.changed)
	v.changed = make(chan struct{})
}

func num(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// valueHandler serves GET /value and POST /value?set=N (and ?target=N), /metrics and /healthz.
func valueHandler(v *value, check func(*http.Request) (string, error)) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/value", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost || r.Method == http.MethodPut {
			var n, t *float64
			for key, dst := range map[string]**float64{"set": &n, "target": &t} {
				if s := r.URL.Query().Get(key); s != "" {
					f, err := strconv.ParseFloat(s, 64)
					if err != nil {
						http.Error(w, key+" wants a number", http.StatusBadRequest)
						return
					}
					*dst = &f
				}
			}
			v.set(n, t)
			nv, tv, _ := v.get()
			log.Printf("value set: backlog %s, target %s", num(nv), num(tv))
			fmt.Fprintf(w, "backlog %s, target %s\n", num(nv), num(tv))
			return
		}
		who := ""
		if check != nil {
			var err error
			if who, err = check(r); err != nil {
				log.Printf("refused GET /value from %s: %v", clientIP(r), err)
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
		}
		n, _, _ := v.get()
		if who != "" {
			log.Printf("GET /value from %s: backlog %s (%s)", clientIP(r), num(n), who)
		} else {
			log.Printf("GET /value from %s: backlog %s", clientIP(r), num(n))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, "{\"backlog\":%s}\n", num(n))
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		n, _, _ := v.get()
		fmt.Fprintf(w, "# HELP demo_backlog The backlog keda-demo was told to report.\n# TYPE demo_backlog gauge\n"+
			"demo_backlog %s\n", num(n))
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	return mux
}

func clientIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

func serve(ctx context.Context, addr string, h http.Handler) error {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	log.Printf("listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func valueServer(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("value-server", flag.ExitOnError)
	addr := fs.String("listen", ":8080", "HTTP address")
	start := fs.Float64("value", 0, "starting backlog")
	audience := fs.String("audience", "", "require a bearer token the API server accepts for this audience")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var check func(*http.Request) (string, error)
	if *audience != "" {
		tr, err := newTokenReviewer()
		if err != nil {
			return err
		}
		check = func(r *http.Request) (string, error) {
			tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || tok == "" {
				return "", errors.New("no bearer token")
			}
			return tr.review(r.Context(), tok, *audience)
		}
		log.Printf("tokens must be valid for audience %q", *audience)
	}
	return serve(ctx, *addr, valueHandler(newValue(*start, 0), check))
}

// tokenReviewer asks the API server whether a bearer token is good for an audience (a TokenReview), with the
// Pod's own service account. It needs RBAC to create tokenreviews (system:auth-delegator has it).
type tokenReviewer struct {
	url    string
	client *http.Client
}

func newTokenReviewer() (*tokenReviewer, error) {
	const sa = "/var/run/secrets/kubernetes.io/serviceaccount/"
	ca, err := os.ReadFile(sa + "ca.crt")
	if err != nil {
		return nil, fmt.Errorf("not in a Pod with a service account: %w", err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	h, p := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	return &tokenReviewer{
		url: "https://" + net.JoinHostPort(h, p) + "/apis/authentication.k8s.io/v1/tokenreviews",
		client: &http.Client{Timeout: 10 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}},
	}, nil
}

func (t *tokenReviewer) review(ctx context.Context, token, audience string) (string, error) {
	own, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil {
		return "", err
	}
	body, _ := json.Marshal(map[string]any{"apiVersion": "authentication.k8s.io/v1", "kind": "TokenReview",
		"spec": map[string]any{"token": token, "audiences": []string{audience}}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(own)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Status struct {
			Authenticated bool     `json:"authenticated"`
			Audiences     []string `json:"audiences"`
			Error         string   `json:"error"`
			User          struct {
				Username string `json:"username"`
			} `json:"user"`
		} `json:"status"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("TokenReview answer: %w", err)
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("TokenReview %d: %s", resp.StatusCode, out.Message)
	}
	if !out.Status.Authenticated {
		if out.Status.Error != "" {
			return "", errors.New(out.Status.Error)
		}
		return "", fmt.Errorf("token not valid for audience %q", audience)
	}
	aud := strings.Join(out.Status.Audiences, ",")
	return fmt.Sprintf("token ok: %s, aud %s", out.Status.User.Username, aud), nil
}

// The external scaler

type extScaler struct {
	es.UnimplementedExternalScalerServer
	v *value
}

func (s *extScaler) target(ref *es.ScaledObjectRef) float64 {
	if _, t, _ := s.v.get(); t > 0 {
		return t
	}
	if f, err := strconv.ParseFloat(ref.GetScalerMetadata()["target"], 64); err == nil && f > 0 {
		return f
	}
	return 5
}

func refName(ref *es.ScaledObjectRef) string {
	return ref.GetNamespace() + "/" + ref.GetName()
}

func (s *extScaler) IsActive(_ context.Context, ref *es.ScaledObjectRef) (*es.IsActiveResponse, error) {
	n, _, _ := s.v.get()
	log.Printf("IsActive %s: %t (backlog %s)", refName(ref), n > 0, num(n))
	return &es.IsActiveResponse{Result: n > 0}, nil
}

func (s *extScaler) StreamIsActive(ref *es.ScaledObjectRef, st es.ExternalScaler_StreamIsActiveServer) error {
	log.Printf("StreamIsActive %s: stream open", refName(ref))
	for {
		n, _, changed := s.v.get()
		if err := st.Send(&es.IsActiveResponse{Result: n > 0}); err != nil {
			return err
		}
		log.Printf("StreamIsActive %s: pushed %t (backlog %s)", refName(ref), n > 0, num(n))
		select {
		case <-changed:
		case <-st.Context().Done():
			log.Printf("StreamIsActive %s: stream closed", refName(ref))
			return nil
		}
	}
}

func (s *extScaler) spec(ref *es.ScaledObjectRef) *es.GetMetricSpecResponse {
	t := s.target(ref)
	return &es.GetMetricSpecResponse{MetricSpecs: []*es.MetricSpec{
		{MetricName: "backlog", TargetSize: int64(t), TargetSizeFloat: t}}}
}

func (s *extScaler) GetMetricSpec(_ context.Context, ref *es.ScaledObjectRef) (
	*es.GetMetricSpecResponse, error) {
	log.Printf("GetMetricSpec %s: backlog, target %s", refName(ref), num(s.target(ref)))
	return s.spec(ref), nil
}

func (s *extScaler) StreamMetricSpec(ref *es.ScaledObjectRef,
	st es.ExternalScaler_StreamMetricSpecServer) error {
	log.Printf("StreamMetricSpec %s: stream open", refName(ref))
	last := -1.0
	for {
		_, _, changed := s.v.get()
		if t := s.target(ref); t != last {
			if err := st.Send(s.spec(ref)); err != nil {
				return err
			}
			log.Printf("StreamMetricSpec %s: pushed target %s", refName(ref), num(t))
			last = t
		}
		select {
		case <-changed:
		case <-st.Context().Done():
			return nil
		}
	}
}

func (s *extScaler) GetMetrics(_ context.Context, req *es.GetMetricsRequest) (*es.GetMetricsResponse, error) {
	n, _, _ := s.v.get()
	log.Printf("GetMetrics %s: %s %s", refName(req.GetScaledObjectRef()), req.GetMetricName(), num(n))
	return &es.GetMetricsResponse{MetricValues: []*es.MetricValue{
		{MetricName: req.GetMetricName(), MetricValue: int64(n), MetricValueFloat: n}}}, nil
}

func scalerServer(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("scaler", flag.ExitOnError)
	grpcAddr := fs.String("grpc", ":9090", "gRPC address KEDA calls")
	httpAddr := fs.String("listen", ":8080", "HTTP address that sets the value")
	start := fs.Float64("value", 0, "starting backlog")
	if err := fs.Parse(args); err != nil {
		return err
	}
	v := newValue(*start, 0)
	lis, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		return err
	}
	gs := grpc.NewServer()
	es.RegisterExternalScalerServer(gs, &extScaler{v: v})
	go func() {
		<-ctx.Done()
		gs.GracefulStop()
	}()
	go func() {
		log.Printf("external scaler on %s (gRPC)", *grpcAddr)
		if err := gs.Serve(lis); err != nil {
			log.Printf("gRPC: %v", err)
		}
	}()
	return serve(ctx, *httpAddr, valueHandler(v, nil))
}

// CPU and memory

// burnSink keeps the busy loop from being optimised away.
var burnSink int

func burnServer(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("burn-server", flag.ExitOnError)
	addr := fs.String("listen", ":8080", "HTTP address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var served atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/burn", func(w http.ResponseWriter, r *http.Request) {
		ms, _ := strconv.Atoi(r.URL.Query().Get("ms"))
		if ms <= 0 || ms > 10000 {
			ms = 50
		}
		end := time.Now().Add(time.Duration(ms) * time.Millisecond)
		for time.Now().Before(end) {
			burnSink++
		}
		served.Add(1)
		fmt.Fprintf(w, "burned %dms on %s\n", ms, host())
	})
	var held sync.Mutex
	var holds [][]byte
	mux.HandleFunc("/hold", func(w http.ResponseWriter, r *http.Request) {
		mb, _ := strconv.Atoi(r.URL.Query().Get("mb"))
		d, err := time.ParseDuration(r.URL.Query().Get("for"))
		if mb <= 0 || mb > 1024 || err != nil {
			http.Error(w, "want mb=1..1024 and for=<duration>", http.StatusBadRequest)
			return
		}
		b := make([]byte, mb<<20)
		for i := 0; i < len(b); i += 4096 {
			b[i] = 1
		}
		held.Lock()
		holds = append(holds, b)
		held.Unlock()
		log.Printf("holding %d MiB for %s", mb, d)
		time.AfterFunc(d, func() {
			held.Lock()
			defer held.Unlock()
			for i := range holds {
				if &holds[i][0] == &b[0] {
					holds = append(holds[:i], holds[i+1:]...)
					break
				}
			}
			log.Printf("let go of %d MiB", mb)
		})
		fmt.Fprintf(w, "holding %d MiB for %s on %s\n", mb, d, host())
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "# HELP demo_requests_total Requests keda-demo served on /burn.\n"+
			"# TYPE demo_requests_total counter\ndemo_requests_total %d\n", served.Load())
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	return serve(ctx, *addr, mux)
}

// Load

func load(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("load", flag.ExitOnError)
	url := fs.String("url", "", "URL to GET")
	hostHdr := fs.String("host", "", "Host header to send")
	c := fs.Int("c", 10, "clients at once")
	dur := fs.Duration("for", 30*time.Second, "how long")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *url == "" {
		return errors.New("-url is required")
	}
	client := &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: *c}}
	lctx, cancel := context.WithTimeout(ctx, *dur)
	defer cancel()
	var mu sync.Mutex
	codes := map[string]int{}
	var lat []time.Duration
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < *c; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for lctx.Err() == nil {
				req, _ := http.NewRequestWithContext(lctx, http.MethodGet, *url, nil)
				if *hostHdr != "" {
					req.Host = *hostHdr
				}
				t := time.Now()
				resp, err := client.Do(req)
				code := "error"
				if err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					code = strconv.Itoa(resp.StatusCode)
				} else if lctx.Err() != nil {
					return
				}
				mu.Lock()
				codes[code]++
				lat = append(lat, time.Since(t))
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	pct := func(p float64) time.Duration {
		if len(lat) == 0 {
			return 0
		}
		return lat[int(p*float64(len(lat)-1))].Round(time.Millisecond)
	}
	var cs []string
	for k, n := range codes {
		cs = append(cs, fmt.Sprintf("%s:%d", k, n))
	}
	sort.Strings(cs)
	log.Printf("load: %d requests in %s from %d clients, codes %s, p50 %s p99 %s",
		len(lat), time.Since(start).Round(time.Second), *c, strings.Join(cs, " "), pct(0.5), pct(0.99))
	return nil
}

// A web app for the HTTP Add-on

func serveApp(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("listen", ":8080", "HTTP address")
	delay := fs.Duration("startup-delay", 0, "wait this long before listening (a slow start)")
	msg := fs.String("message", "hello", "what / answers")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *delay > 0 {
		log.Printf("starting, listening in %s", *delay)
		sleepCtx(ctx, *delay)
		if ctx.Err() != nil {
			return nil
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s from %s\n", *msg, host())
	})
	mux.HandleFunc("/delay", func(w http.ResponseWriter, r *http.Request) {
		ms, _ := strconv.Atoi(r.URL.Query().Get("ms"))
		if ms < 0 || ms > 60000 {
			ms = 0
		}
		sleepCtx(r.Context(), time.Duration(ms)*time.Millisecond)
		fmt.Fprintf(w, "%s from %s after %dms\n", *msg, host(), ms)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	return serve(ctx, *addr, mux)
}
