package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bluesky-social/indigo/events"
	"github.com/bluesky-social/indigo/events/schedulers/autoscaling"
	"github.com/ericvolp12/bsky-experiments/pkg/tracing"
	"github.com/ericvolp12/jetstream/pkg/consumer"
	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"golang.org/x/exp/slog"

	"github.com/urfave/cli/v2"
)

func main() {
	app := cli.App{
		Name:    "jetstream",
		Usage:   "atproto firehose translation service",
		Version: "0.0.1",
	}

	app.Flags = []cli.Flag{
		&cli.StringFlag{
			Name:    "ws-url",
			Usage:   "full websocket path to the ATProto SubscribeRepos XRPC endpoint",
			Value:   "wss://bsky.social/xrpc/com.atproto.sync.subscribeRepos",
			EnvVars: []string{"WS_URL"},
		},
		&cli.IntFlag{
			Name:    "worker-count",
			Usage:   "number of workers to process events",
			Value:   10,
			EnvVars: []string{"WORKER_COUNT"},
		},
		&cli.IntFlag{
			Name:    "max-queue-size",
			Usage:   "max number of events to queue",
			Value:   10,
			EnvVars: []string{"MAX_QUEUE_SIZE"},
		},
		&cli.IntFlag{
			Name:    "port",
			Usage:   "port to serve metrics on",
			Value:   8080,
			EnvVars: []string{"PORT"},
		},
		&cli.StringFlag{
			Name:    "cursor-file",
			Usage:   "path to the cursor file",
			Value:   "./cursor.json",
			EnvVars: []string{"CURSOR_FILE"},
		},
	}

	app.Action = Jetstream

	err := app.Run(os.Args)
	if err != nil {
		log.Fatal(err)
	}
}

var tracer = otel.Tracer("Jetstream")

// Jetstream is the main function for jetstream
func Jetstream(cctx *cli.Context) error {
	ctx := cctx.Context

	// Create a channel that will be closed when we want to stop the application
	// Usually when a critical routine returns an error
	kill := make(chan struct{})

	// Trap SIGINT to trigger a shutdown.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)

	log := slog.New(slog.NewJSONHandler(os.Stdout))
	slog.SetDefault(log)

	log.Info("starting jetstream")

	u, err := url.Parse(cctx.String("ws-url"))
	if err != nil {
		return fmt.Errorf("failed to parse ws-url: %w", err)
	}

	// Registers a tracer Provider globally if the exporter endpoint is set
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" {
		log.Info("initializing tracer...")
		shutdown, err := tracing.InstallExportPipeline(ctx, "Jetstream", 0.01)
		if err != nil {
			return fmt.Errorf("failed to initialize tracer: %w", err)
		}
		defer func() {
			if err := shutdown(ctx); err != nil {
				log.Error("failed to shutdown tracer", "error", err)
			}
		}()
	}

	s := NewServer()

	c, err := consumer.NewConsumer(
		ctx,
		u.String(),
		cctx.String("cursor-file"),
		s.Emit,
	)
	if err != nil {
		return fmt.Errorf("failed to create consumer: %w", err)
	}

	schedSettings := autoscaling.DefaultAutoscaleSettings()
	scheduler := autoscaling.NewScheduler(schedSettings, "prod-firehose", c.HandleStreamEvent)

	// Start a goroutine to manage the cursor, saving the current cursor every 5 seconds.
	shutdownCursorManager := make(chan struct{})
	cursorManagerShutdown := make(chan struct{})
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		log := log.With("source", "cursor_manager")

		for {
			select {
			case <-shutdownCursorManager:
				log.Info("shutting down cursor manager")
				err := c.WriteCursor(ctx)
				if err != nil {
					log.Error("failed to write cursor", "error", err)
				}
				log.Info("cursor manager shut down successfully")
				close(cursorManagerShutdown)
				return
			case <-ticker.C:
				err := c.WriteCursor(ctx)
				if err != nil {
					log.Error("failed to write cursor", "error", err)
				}
			}
		}
	}()

	// Start a goroutine to manage the liveness checker, shutting down if no events are received for 15 seconds
	shutdownLivenessChecker := make(chan struct{})
	livenessCheckerShutdown := make(chan struct{})
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		lastSeq := int64(0)
		log := log.With("source", "liveness_checker")

		for {
			select {
			case <-shutdownLivenessChecker:
				log.Info("shutting down liveness checker")
				close(livenessCheckerShutdown)
				return
			case <-ticker.C:
				seq, _ := c.Progress.Get()
				if seq == lastSeq {
					log.Error("no new events in last 15 seconds, shutting down for docker to restart me", "seq", seq)
					close(kill)
				} else {
					log.Info("successful liveness check", "seq", seq)
					lastSeq = seq
				}
			}
		}
	}()

	e := echo.New()
	e.GET("/", func(c echo.Context) error {
		return c.String(http.StatusOK, "Hello, World!")
	})
	e.GET("/metrics", echo.WrapHandler(promhttp.Handler()))
	e.GET("/subscribe", s.HandleSubscribe)

	metricServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", cctx.Int("port")),
		Handler: e,
	}

	// Startup metrics server
	shutdownMetrics := make(chan struct{})
	metricsShutdown := make(chan struct{})
	go func() {
		logger := log.With("source", "metrics_server")

		logger.Info("metrics server listening", "port", cctx.Int("port"))

		go func() {
			if err := metricServer.ListenAndServe(); err != http.ErrServerClosed {
				logger.Error("failed to start metrics server", "error", err)
			}
		}()
		<-shutdownMetrics
		if err := metricServer.Shutdown(ctx); err != nil {
			logger.Error("failed to shutdown metrics server", "error", err)
		}
		logger.Info("metrics server shut down")
		close(metricsShutdown)
	}()

	if c.Progress.LastSeq >= 0 {
		u.RawQuery = fmt.Sprintf("cursor=%d", c.Progress.LastSeq)
	}

	log.Info(fmt.Sprintf("connecting to WebSocket at: %s", u.String()))
	con, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		log.Error("failed to connect to websocket", "error", err)
		return err
	}
	defer con.Close()

	shutdownRepoStream := make(chan struct{})
	repoStreamShutdown := make(chan struct{})
	go func() {
		ctx := context.Background()
		ctx, cancel := context.WithCancel(ctx)
		go func() {
			err = events.HandleRepoStream(ctx, con, scheduler)
			if !errors.Is(err, context.Canceled) {
				log.Info("HandleRepoStream returned unexpectedly, killing jetstream", "error", err)
				close(kill)
			} else {
				log.Info("HandleRepoStream closed on context cancel")
			}
			close(repoStreamShutdown)
		}()
		<-shutdownRepoStream
		cancel()
	}()

	select {
	case <-signals:
		log.Info("shutting down on signal")
	case <-ctx.Done():
		log.Info("shutting down on context done")
	case <-kill:
		log.Info("shutting down on kill")
	}

	log.Info("shutting down, waiting for workers to clean up...")
	close(shutdownRepoStream)
	close(shutdownLivenessChecker)
	close(shutdownCursorManager)
	close(shutdownMetrics)

	<-repoStreamShutdown
	<-livenessCheckerShutdown
	<-cursorManagerShutdown
	<-metricsShutdown
	log.Info("shut down successfully")

	return nil
}