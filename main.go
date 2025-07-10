package main

import (
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttpadaptor"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"golang.org/x/sync/singleflight"
)

const (
	cacheTTL     = 10 * time.Second
	rateLimit    = 10
	rateWindow   = 60 * time.Second
	shardCount   = 256
	listenAddr   = ":8080"
	rpcTimeout   = 4 * time.Second
)

var (
	rdb     *redis.Client
	tmdb    *mongo.Client
	mcoll   *mongo.Collection
	rpcCli  *rpc.Client
	sfg     singleflight.Group
	shards  [shardCount]shard
	metrics = initMetrics()
)

type cacheEntry struct {
	Value   float64
	Expires int64
}

type shard struct {
	sync.RWMutex
	entries map[string]cacheEntry
}

type balanceRequest struct {
	Wallets []string `json:"wallets"`
}

type balanceResponse map[string]float64

func main() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	initRedis()
	initMongo(ctx)
	initShards()
	rpcUrl, ok := os.LookupEnv("RPC_URL")
	if !ok {
		log.Info().Msg("no RPC_URL set")
	}
	log.Debug().Msg(rpcUrl)
	rpcCli = rpc.New(rpcUrl)

	server := &fasthttp.Server{Handler: router}

	go func() {
		log.Info().Str("listen", listenAddr).Msg("starting server")
		if err := server.ListenAndServe(listenAddr); err != nil {
			log.Fatal().Err(err).Msg("server failed")
		}
	}()

	<-ctx.Done()
	log.Info().Msg("shutting down")
	_ = tmdb.Disconnect(context.Background())
	_ = server.Shutdown()
}

func router(ctx *fasthttp.RequestCtx) {
	switch string(ctx.Path()) {
	case "/metrics":
		fasthttpadaptor.NewFastHTTPHandler(promhttp.Handler())(ctx)
	case "/api/get-balance":
		if ctx.IsPost() {
			handleBalance(ctx)
		} else {
			ctx.SetStatusCode(fasthttp.StatusMethodNotAllowed)
		}
	default:
		ctx.SetStatusCode(fasthttp.StatusNotFound)
	}
}

func initRedis() {
	rdb = redis.NewClient(&redis.Options{
		Addr: os.Getenv("REDIS_ADDR"),
	})
}

func initMongo(ctx context.Context) {
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(os.Getenv("MONGODB_URI")))
	if err != nil {
		log.Fatal().Err(err).Msg("mongo connect failed")
	}
	tmdb = client
	mcoll = client.Database(os.Getenv("MONGODB_DB")).Collection(os.Getenv("MONGODB_COL"))
}

func initShards() {
	for i := range shards {
		shards[i].entries = make(map[string]cacheEntry)
	}
}

func shardFor(key string) *shard {
	h := sha1.Sum([]byte(key))
	return &shards[binary.BigEndian.Uint16(h[:2])%shardCount]
}

func overLimit(ip string) bool {
	ctx := context.Background()
	key := "rate:" + ip
	pipe := rdb.Pipeline()
	incr := pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, rateWindow)
	_, err := pipe.Exec(ctx)
	if err != nil {
		log.Error().Err(err).Str("ip", ip).Msg("rate limit check failed")
		return false
	}
	return incr.Val() > rateLimit
}

func validateAPIKey(ctx context.Context, key string) bool {
	if key == "" {
		return false
	}
	res := mcoll.FindOne(ctx, bson.M{"key": key})
	return res.Err() == nil
}

func handleBalance(ctx *fasthttp.RequestCtx) {
	ip := getIP(ctx)
	apiKey := string(ctx.Request.Header.Peek("X-API-Key"))

	if !validateAPIKey(context.Background(), apiKey) {
		ctx.SetStatusCode(fasthttp.StatusUnauthorized)
		return
	}
	if overLimit(ip) {
		ctx.SetStatusCode(fasthttp.StatusTooManyRequests)
		ctx.Response.Header.Set("Retry-After", fmt.Sprintf("%d", int(rateWindow.Seconds())))
		return
	}

	var req balanceRequest
	if err := json.Unmarshal(ctx.PostBody(), &req); err != nil || len(req.Wallets) == 0 {
		ctx.SetStatusCode(fasthttp.StatusBadRequest)
		return
	}
	if len(req.Wallets) > 100 {
		ctx.SetStatusCode(fasthttp.StatusRequestHeaderFieldsTooLarge)
		return
	}

	res := make(balanceResponse)
	sem := make(chan struct{}, 16)
	wg := sync.WaitGroup{}

	for _, wallet := range req.Wallets {
		wallet = strings.TrimSpace(wallet)
		if wallet == "" {
			continue
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(w string) {
			defer wg.Done()
			defer func() { <-sem }()
			bal, err := getBalance(ctx, w)
			if err != nil {
				log.Warn().Err(err).Str("wallet", w).Msg("failed to fetch balance")
				return
			}
			log.Info().Str("wallet", w).Float64("balance", bal).Msg("fetched balance")
			res[w] = bal
		}(wallet)
	}
	wg.Wait()

	out, _ := json.Marshal(res)
	ctx.SetContentType("application/json")
	ctx.Write(out)
}

func getBalance(_ *fasthttp.RequestCtx, wallet string) (float64, error) {
	s := shardFor(wallet)
	now := time.Now().UnixNano()
	s.RLock()
	if e, ok := s.entries[wallet]; ok && now < e.Expires {
		s.RUnlock()
		metrics.cacheHits.Inc()
		return e.Value, nil
	}
	s.RUnlock()

	val, err, _ := sfg.Do(wallet, func() (interface{}, error) {
		pk, err := solana.PublicKeyFromBase58(wallet)
		if err != nil {
			return nil, fmt.Errorf("invalid wallet: %w", err)
		}
		rctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
		defer cancel()
		out, err := rpcCli.GetBalance(rctx, pk, rpc.CommitmentFinalized)
		if err != nil {
			return nil, fmt.Errorf("rpc failed: %w", err)
		}
		bal := float64(out.Value) / float64(solana.LAMPORTS_PER_SOL)
		s.Lock()
		s.entries[wallet] = cacheEntry{bal, time.Now().Add(cacheTTL).UnixNano()}
		s.Unlock()
		metrics.rpcCalls.Inc()
		return bal, nil
	})
	if err != nil {
		metrics.errors.Inc()
		return 0, err
	}
	return val.(float64), nil
}

func getIP(ctx *fasthttp.RequestCtx) string {
	if xff := ctx.Request.Header.Peek("X-Forwarded-For"); len(xff) > 0 {
		return string(xff)
	}
	return ctx.RemoteIP().String()
}

type metricSet struct {
	rpcCalls  prometheus.Counter
	errors    prometheus.Counter
	cacheHits prometheus.Counter
}

func initMetrics() *metricSet {
	m := &metricSet{
		rpcCalls: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "rpc_calls_total", Help: "Total Solana RPC calls",
		}),
		errors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "errors_total", Help: "Total errors",
		}),
		cacheHits: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "cache_hits_total", Help: "Cache hits",
		}),
	}
	prometheus.MustRegister(m.rpcCalls, m.errors, m.cacheHits)
	return m
}