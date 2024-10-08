package main

import (
	"context"
	"errors"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

var (
	registry        = prometheus.NewRegistry()
	packetsReceived = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "udp_fwd_packets_received_total",
		Help: "Total number of UDP packets received",
	})
	packetsForwarded = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "udp_fwd_packets_forwarded_total",
		Help: "Total number of UDP packets forwarded",
	}, []string{"destination"})
	bytesForwarded = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "udp_fwd_bytes_forwarded_total",
		Help: "Total number of bytes forwarded",
	}, []string{"destination"})
)

func init() {
	registry.MustRegister(packetsReceived)
	registry.MustRegister(packetsForwarded)
	registry.MustRegister(bytesForwarded)
}

type DestinationConn struct {
	conn net.Conn
	addr string
}

var bufferPool = sync.Pool{
	New: func() interface{} {
		return make([]byte, 65536)
	},
}

var debugLog int32 = 1

func setDebugLog(value bool) {
	if value {
		atomic.StoreInt32(&debugLog, 1)
	} else {
		atomic.StoreInt32(&debugLog, 0)
	}
}

func isDebugLog() bool {
	return atomic.LoadInt32(&debugLog) == 1
}

func main() {
	setupLogger()

	enableGoMetrics := os.Getenv("PROM_GO_METRICS")
	if enableGoMetrics == "true" {
		registry.MustRegister(collectors.NewGoCollector())
		log.Info().Msg("Go metrics enabled")
	} else {
		if enableGoMetrics == "" {
			log.Info().Msg("PROM_GO_METRICS not set. Go Prometheus metrics disabled")
		} else {
			log.Info().Msg("Go metrics disabled")
		}
	}

	listenPort := os.Getenv("UDP_LISTEN_PORT")
	if listenPort == "" {
		log.Fatal().Msg("UDP_LISTEN_PORT environment variable is not set")
	}

	destinations := strings.Split(os.Getenv("UDP_DESTINATIONS"), ",")
	if len(destinations) == 0 {
		log.Fatal().Msg("UDP_DESTINATIONS environment variable is not set or empty")
	}

	destConns := make([]DestinationConn, len(destinations))
	for i, dest := range destinations {
		addr, err := net.ResolveUDPAddr("udp", dest)
		if err != nil {
			log.Fatal().Err(err).Str("destination", dest).Msg("Failed to resolve destination address")
		}
		conn, err := net.DialUDP("udp", nil, addr)
		if err != nil {
			log.Fatal().Err(err).Str("destination", dest).Msg("Failed to connect to destination")
		}
		destConns[i] = DestinationConn{conn: conn, addr: dest}
		packetsForwarded.WithLabelValues(dest)
		bytesForwarded.WithLabelValues(dest)
	}

	addr, err := net.ResolveUDPAddr("udp", ":"+listenPort)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to resolve UDP address")
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to start UDP server")
	}

	log.Info().Str("port", listenPort).Msg("UDP server started")

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		handleConnections(ctx, conn, destConns)
	}()

	srv := &http.Server{Addr: ":8080"}
	http.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	go func() {
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Error().Err(err).Msg("Metrics server failed")
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Info().Msg("Shutting down...")

	cancel()

	if err := conn.Close(); err != nil {
		log.Error().Err(err).Msg("Error closing UDP listener")
	}

	for _, destConn := range destConns {
		if err := destConn.conn.Close(); err != nil {
			log.Error().Err(err).Str("destination", destConn.addr).Msg("Error closing destination connection")
		}
	}

	ctxShutdown, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	if err := srv.Shutdown(ctxShutdown); err != nil {
		log.Error().Err(err).Msg("Metrics server shutdown failed")
	}

	wg.Wait()

	log.Info().Msg("Server stopped")
}

func setupLogger() {
	logLevel := os.Getenv("LOG_LEVEL")
	if logLevel == "" {
		logLevel = "info"
	}

	level, err := zerolog.ParseLevel(logLevel)
	if err != nil {
		level = zerolog.InfoLevel
	}

	zerolog.SetGlobalLevel(level)

	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Logger = zerolog.New(os.Stdout).With().Timestamp().Logger()
}

func handleConnections(ctx context.Context, conn UDPConn, destConns []DestinationConn) {
	for {
		select {
		case <-ctx.Done():
			log.Debug().Msg("Stopping UDP packet handling")
			return
		default:
			buffer := bufferPool.Get().([]byte)
			n, remoteAddr, err := readPacket(ctx, conn, buffer)
			if err != nil {
				bufferPool.Put(buffer)
				if err == errContextCanceled {
					return
				}
				continue
			}

			packetsReceived.Inc()
			log.Debug().Str("remote_addr", remoteAddr.String()).Int("bytes", n).Msg("Received packet")

			packetCopy := make([]byte, n)
			copy(packetCopy, buffer[:n])

			forwardPackets(packetCopy, destConns)
			bufferPool.Put(buffer)
		}
	}
}

func readPacket(ctx context.Context, conn UDPConn, buffer []byte) (int, *net.UDPAddr, error) {
	for {
		err := conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		if err != nil {
			log.Error().Err(err).Msg("Error setting read deadline")
			continue
		}

		n, remoteAddr, err := conn.ReadFromUDP(buffer)
		if err != nil {
			if ctx.Err() != nil {
				return 0, nil, errContextCanceled
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			log.Error().Err(err).Msg("Error reading UDP packet")
			continue
		}

		return n, remoteAddr, nil
	}
}

func forwardPackets(packet []byte, destConns []DestinationConn) {
	for _, destConn := range destConns {
		go forwardPacket(packet, destConn)
	}
}

func forwardPacket(packet []byte, destConn DestinationConn) {
	var n int
	var err error

	if isDebugLog() {
		log.Debug().Str("destination", destConn.addr).Int("packet_size", len(packet)).Msg("Attempting to forward packet")
	}

	if udpConn, ok := destConn.conn.(*net.UDPConn); ok {
		n, err = udpConn.Write(packet)
	} else {
		n, err = destConn.conn.Write(packet)
	}

	if err != nil {
		if isDebugLog() {
			log.Error().Err(err).Str("destination", destConn.addr).Int("packet_size", len(packet)).Msg("Error forwarding packet")
		}
		return
	}

	if n != len(packet) && isDebugLog() {
		log.Warn().Str("destination", destConn.addr).Int("sent", n).Int("expected", len(packet)).Msg("Incomplete packet sent")
	}

	packetsForwarded.WithLabelValues(destConn.addr).Inc()
	bytesForwarded.WithLabelValues(destConn.addr).Add(float64(n))

	if isDebugLog() {
		log.Debug().Str("destination", destConn.addr).Int("bytes", n).Msg("Forwarded packet")
	}
}

var errContextCanceled = errors.New("context canceled")
