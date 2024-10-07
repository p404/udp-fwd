package main

import (
	"context"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"net/http"
)

var (
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
	prometheus.MustRegister(packetsReceived)
	prometheus.MustRegister(packetsForwarded)
	prometheus.MustRegister(bytesForwarded)
}

type DestinationConn struct {
	conn net.Conn
	addr string
}

func main() {
	setupLogger()

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
		conn, err := net.Dial("udp", dest)
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

	// Start UDP handling
	wg.Add(1)
	go func() {
		defer wg.Done()
		handleConnections(ctx, conn, destConns)
	}()

	// Start metrics server
	srv := &http.Server{Addr: ":8080"}
	http.Handle("/metrics", promhttp.Handler())
	go func() {
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Error().Err(err).Msg("Metrics server failed")
		}
	}()

	// Wait for interrupt signal
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Info().Msg("Shutting down...")

	// Shutdown gracefully
	cancel()

	// Close UDP listener
	if err := conn.Close(); err != nil {
		log.Error().Err(err).Msg("Error closing UDP listener")
	}

	// Close destination connections
	for _, destConn := range destConns {
		if err := destConn.conn.Close(); err != nil {
			log.Error().Err(err).Str("destination", destConn.addr).Msg("Error closing destination connection")
		}
	}

	// Shutdown metrics server
	ctxShutdown, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	if err := srv.Shutdown(ctxShutdown); err != nil {
		log.Error().Err(err).Msg("Metrics server shutdown failed")
	}

	// Wait for goroutines to finish
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

	// Configure zerolog to output JSON
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Logger = zerolog.New(os.Stdout).With().Timestamp().Logger()
}

func handleConnections(ctx context.Context, conn UDPConn, destConns []DestinationConn) {
	buffer := make([]byte, 1024)

	for {
		select {
		case <-ctx.Done():
			return
		default:
			err := conn.SetReadDeadline(time.Now().Add(1 * time.Second))
			if err != nil {
				log.Error().Err(err).Msg("Error setting read deadline")
				continue
			}

			n, remoteAddr, err := conn.ReadFromUDP(buffer)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue
				}
				log.Error().Err(err).Msg("Error reading UDP packet")
				continue
			}

			packetsReceived.Inc()
			log.Debug().Str("remote_addr", remoteAddr.String()).Msg("Received packet")

			for _, destConn := range destConns {
				go forwardPacket(buffer[:n], destConn)
			}
		}
	}
}

func forwardPacket(packet []byte, destConn DestinationConn) {
	var n int
	var err error

	log.Debug().Str("destination", destConn.addr).Int("packet_size", len(packet)).Msg("Attempting to forward packet")

	// Check if the connection is a UDP connection
	if udpConn, ok := destConn.conn.(*net.UDPConn); ok {
		// For ListenUDP connections, we need to use WriteToUDP
		addr, err := net.ResolveUDPAddr("udp", destConn.addr)
		if err != nil {
			log.Error().Err(err).Str("destination", destConn.addr).Msg("Failed to resolve UDP address")
			return
		}
		n, err = udpConn.WriteToUDP(packet, addr)
	} else {
		n, err = destConn.conn.Write(packet)
	}

	if err != nil {
		log.Error().Err(err).Str("destination", destConn.addr).Int("packet_size", len(packet)).Msg("Error forwarding packet")
		return
	}

	packetsForwarded.WithLabelValues(destConn.addr).Inc()
	bytesForwarded.WithLabelValues(destConn.addr).Add(float64(n))
	log.Debug().Str("destination", destConn.addr).Int("bytes", n).Msg("Forwarded packet")
}
