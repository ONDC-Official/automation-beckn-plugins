package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

func main() {
	ctx := context.Background()
	cfg, err := loadConfig()
	if err != nil {
		log.Errorf(ctx, err, "automation-recorder: invalid config")
		os.Exit(2)
	}

	rdb := newRedisClient(cfg.RedisAddr)
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Errorf(ctx, err, "automation-recorder: failed to connect to redis")
		os.Exit(2)
	}

	// HTTP API (form endpoint)
	if cfg.HTTPListenAddr != "" {
		go func() {
			log.Infof(ctx, "automation-recorder: http listening on %s", cfg.HTTPListenAddr)
			if err := http.ListenAndServe(cfg.HTTPListenAddr, newHTTPMux(rdb)); err != nil {
				log.Errorf(ctx, err, "automation-recorder: http serve failed")
				os.Exit(1)
			}
		}()
	}

	lsn, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		log.Errorf(ctx, err, "automation-recorder: listen failed")
		os.Exit(2)
	}

	dispatcher := newAsyncDispatcher(ctx, cfg.AsyncQueueSize, cfg.AsyncWorkerCount, cfg.DropOnQueueFull)

	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(recoveryUnaryInterceptor),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    2 * time.Minute,
			Timeout: 20 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             30 * time.Second,
			PermitWithoutStream: true,
		}),
	)

	httpClient := &http.Client{Timeout: 10 * time.Second}
	registerAuditService(srv, &recorderServer{rdb: rdb, cfg: cfg, httpClient: httpClient, async: dispatcher})

	log.Infof(ctx, "automation-recorder: listening on %s", cfg.ListenAddr)
	if err := srv.Serve(lsn); err != nil {
		log.Errorf(ctx, err, "automation-recorder: grpc serve failed")
		os.Exit(1)
	}
}
