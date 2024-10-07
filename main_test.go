package main

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
)

//go:generate mockgen -source=types.go -destination=mock_udp_conn.go -package=main

func TestMain(m *testing.M) {
	// Setup
	os.Setenv("UDP_LISTEN_PORT", "5000")
	os.Setenv("UDP_DESTINATIONS", "localhost:6000,localhost:6001")
	os.Setenv("LOG_LEVEL", "error")

	// Run tests
	code := m.Run()

	// Teardown
	os.Unsetenv("UDP_LISTEN_PORT")
	os.Unsetenv("UDP_DESTINATIONS")
	os.Unsetenv("LOG_LEVEL")

	os.Exit(code)
}

func TestHandleConnections(t *testing.T) {
	// Setup
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockConn := NewMockUDPConn(ctrl)
	destinations := []string{"localhost:6000", "localhost:6001"}
	ctx, cancel := context.WithCancel(context.Background())

	// Expectations
	packet := []byte("test packet")
	remoteAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 54321}
	mockConn.EXPECT().SetReadDeadline(gomock.Any()).Return(nil).AnyTimes()
	mockConn.EXPECT().ReadFromUDP(gomock.Any()).DoAndReturn(func(b []byte) (int, *net.UDPAddr, error) {
		copy(b, packet)
		return len(packet), remoteAddr, nil
	}).AnyTimes()

	// Run the function in a goroutine
	go handleConnections(ctx, mockConn, destinations)

	// Wait a bit for the goroutine to process
	time.Sleep(100 * time.Millisecond)

	// Signal to stop
	cancel()

	// Wait for the goroutine to finish
	time.Sleep(100 * time.Millisecond)

	// Assertions
	// Note: We can't easily assert on forwarded packets in this test setup
	// That would require more complex mocking of net.Dial and net.Conn
}

func TestForwardPacket(t *testing.T) {
	// Setup mock UDP server
	addr, err := net.ResolveUDPAddr("udp", "localhost:0")
	assert.NoError(t, err)

	conn, err := net.ListenUDP("udp", addr)
	assert.NoError(t, err)
	defer conn.Close()

	// Run forwardPacket in a goroutine
	packet := []byte("test packet")
	go forwardPacket(packet, conn.LocalAddr().String())

	// Read from the mock server
	buffer := make([]byte, 1024)
	conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	n, _, err := conn.ReadFromUDP(buffer)

	// Assertions
	assert.NoError(t, err)
	assert.Equal(t, packet, buffer[:n])
}
