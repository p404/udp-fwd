package main

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/rs/zerolog"
	"github.com/schollz/progressbar/v3"
	"github.com/stretchr/testify/assert"
)

func TestMain(m *testing.M) {
	os.Setenv("UDP_LISTEN_PORT", "5000")
	os.Setenv("UDP_DESTINATIONS", "localhost:6000,localhost:6001")
	os.Setenv("LOG_LEVEL", "debug")

	code := m.Run()

	os.Unsetenv("UDP_LISTEN_PORT")
	os.Unsetenv("UDP_DESTINATIONS")
	os.Unsetenv("LOG_LEVEL")

	os.Exit(code)
}

func TestHandleConnections(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockConn := NewMockUDPConn(ctrl)

	destConns := make([]DestinationConn, 2)
	for i := range destConns {
		serverAddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
		assert.NoError(t, err)
		serverConn, err := net.ListenUDP("udp", serverAddr)
		assert.NoError(t, err)
		defer serverConn.Close()

		clientConn, err := net.DialUDP("udp", nil, serverConn.LocalAddr().(*net.UDPAddr))
		assert.NoError(t, err)
		defer clientConn.Close()

		destConns[i] = DestinationConn{
			conn: clientConn,
			addr: serverConn.LocalAddr().String(),
		}
	}

	ctx, cancel := context.WithCancel(context.Background())

	packet := []byte("test packet")
	remoteAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 54321}
	mockConn.EXPECT().SetReadDeadline(gomock.Any()).Return(nil).AnyTimes()
	mockConn.EXPECT().ReadFromUDP(gomock.Any()).DoAndReturn(func(b []byte) (int, *net.UDPAddr, error) {
		n := copy(b, packet)
		return n, remoteAddr, nil
	}).AnyTimes()

	go handleConnections(ctx, mockConn, destConns)

	time.Sleep(100 * time.Millisecond)

	cancel()

	time.Sleep(100 * time.Millisecond)
}

func TestForwardPacket(t *testing.T) {
	addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	assert.NoError(t, err)

	serverConn, err := net.ListenUDP("udp", addr)
	assert.NoError(t, err)
	defer serverConn.Close()

	clientConn, err := net.DialUDP("udp", nil, serverConn.LocalAddr().(*net.UDPAddr))
	assert.NoError(t, err)
	defer clientConn.Close()

	destConn := DestinationConn{
		conn: clientConn,
		addr: clientConn.RemoteAddr().String(),
	}

	packet := []byte("test packet")
	forwardPacket(packet, destConn)

	buffer := make([]byte, 1024)
	serverConn.SetReadDeadline(time.Now().Add(1 * time.Second))
	n, _, err := serverConn.ReadFromUDP(buffer)

	if err != nil {
		t.Logf("Error reading from UDP: %v", err)
	}
	t.Logf("Bytes read: %d", n)
	t.Logf("Buffer content: %v", buffer[:n])

	assert.NoError(t, err)
	assert.Equal(t, packet, buffer[:n])
}

func TestForwardPacketsWithDifferentSizes(t *testing.T) {
	destCount := 2
	servers := make([]*net.UDPConn, destCount)
	destConns := make([]DestinationConn, destCount)

	for i := 0; i < destCount; i++ {
		addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
		assert.NoError(t, err)

		serverConn, err := net.ListenUDP("udp", addr)
		assert.NoError(t, err)
		defer serverConn.Close()

		servers[i] = serverConn

		clientConn, err := net.DialUDP("udp", nil, serverConn.LocalAddr().(*net.UDPAddr))
		assert.NoError(t, err)
		defer clientConn.Close()

		destConns[i] = DestinationConn{
			conn: clientConn,
			addr: clientConn.RemoteAddr().String(),
		}
	}

	testSizes := []int{10, 100, 500, 1000, 1500}

	for _, size := range testSizes {
		t.Run(fmt.Sprintf("PacketSize_%d", size), func(t *testing.T) {
			packet := make([]byte, size)
			for i := range packet {
				packet[i] = byte(i % 256)
			}

			forwardPackets(packet, destConns)

			time.Sleep(100 * time.Millisecond) // Give some time for packets to be forwarded

			for i, server := range servers {
				buffer := make([]byte, 2000)
				server.SetReadDeadline(time.Now().Add(1 * time.Second))
				n, _, err := server.ReadFromUDP(buffer)

				assert.NoError(t, err, "Error reading from UDP for destination %d", i)
				assert.Equal(t, size, n, "Incorrect number of bytes read for destination %d", i)
				assert.Equal(t, packet, buffer[:n], "Incorrect packet content for destination %d", i)

				t.Logf("Destination %d received %d bytes", i, n)
			}
		})
	}
}

func TestHighLoadStatsdMetrics(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping high load test in short mode")
	}

	// Disable all logging for this test
	originalLogLevel := zerolog.GlobalLevel()
	zerolog.SetGlobalLevel(zerolog.Disabled)
	defer zerolog.SetGlobalLevel(originalLogLevel)

	// Setup
	serverAddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	assert.NoError(t, err)
	serverConn, err := net.ListenUDP("udp", serverAddr)
	assert.NoError(t, err)
	defer serverConn.Close()

	clientConn, err := net.DialUDP("udp", nil, serverConn.LocalAddr().(*net.UDPAddr))
	assert.NoError(t, err)
	defer clientConn.Close()

	destConn := DestinationConn{
		conn: clientConn,
		addr: serverConn.LocalAddr().String(),
	}

	// Test parameters
	metricsPerSecond := 100000
	testDuration := 5 * time.Minute
	totalMetrics := metricsPerSecond * int(testDuration.Seconds())

	fmt.Printf("Starting load test: sending %d metrics/second for %v\n", metricsPerSecond, testDuration)
	fmt.Printf("Listening on %s\n", serverConn.LocalAddr().String())

	ctx, cancel := context.WithTimeout(context.Background(), testDuration)
	defer cancel()

	var sentCount, receivedCount int64
	var wg sync.WaitGroup

	// Progress bar
	bar := progressbar.NewOptions(totalMetrics,
		progressbar.OptionSetDescription("Sending metrics"),
		progressbar.OptionSetWidth(50),
		progressbar.OptionThrottle(65*time.Millisecond),
		progressbar.OptionShowCount(),
		progressbar.OptionOnCompletion(func() {
			fmt.Println("\nLoad test completed")
		}),
	)

	// Sending goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for i := 0; i < metricsPerSecond; i++ {
					metric := fmt.Sprintf("test.metric:%d|c", rand.Intn(100))
					forwardPacket([]byte(metric), destConn)
					atomic.AddInt64(&sentCount, 1)
					bar.Add(1)
				}
			}
		}
	}()

	// Receiving goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		buffer := make([]byte, 65536)
		for {
			select {
			case <-ctx.Done():
				return
			default:
				serverConn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
				n, _, err := serverConn.ReadFromUDP(buffer)
				if err != nil {
					if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
						continue
					}
					if ctx.Err() != nil {
						return
					}
					continue
				}
				atomic.AddInt64(&receivedCount, 1)
				assert.True(t, n > 0, "Received empty packet")
			}
		}
	}()

	// Wait for test completion
	wg.Wait()

	// Assert results
	sentFinal := atomic.LoadInt64(&sentCount)
	receivedFinal := atomic.LoadInt64(&receivedCount)
	fmt.Printf("\nTest completed. Sent: %d, Received: %d, Duration: %v\n", sentFinal, receivedFinal, testDuration)
	assert.InDelta(t, totalMetrics, sentFinal, float64(totalMetrics)*0.1, "Incorrect number of metrics sent")
	assert.InDelta(t, sentFinal, receivedFinal, float64(sentFinal)*0.1, "More than 10% packet loss")
}
