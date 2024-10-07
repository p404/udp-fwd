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
	os.Setenv("LOG_LEVEL", "debug") // Set to debug for more detailed logging

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

	// Create real UDP connections for destinations
	destConns := make([]DestinationConn, 2)
	for i := range destConns {
		// Create a local address to listen on
		addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
		assert.NoError(t, err)

		// Use ListenUDP to create a UDP connection
		conn, err := net.ListenUDP("udp", addr)
		assert.NoError(t, err)
		defer conn.Close()

		destConns[i] = DestinationConn{
			conn: conn,
			addr: conn.LocalAddr().String(),
		}
	}

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
	go handleConnections(ctx, mockConn, destConns)

	// Wait a bit for the goroutine to process
	time.Sleep(100 * time.Millisecond)

	// Signal to stop
	cancel()

	// Wait for the goroutine to finish
	time.Sleep(100 * time.Millisecond)

	// Assertions
	// You can add assertions here to check if packets were received by the destination connections
}

func TestForwardPacket(t *testing.T) {
	// Setup mock UDP server
	addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	assert.NoError(t, err)

	serverConn, err := net.ListenUDP("udp", addr)
	assert.NoError(t, err)
	defer serverConn.Close()

	// Create a DestinationConn
	destConn := DestinationConn{
		conn: serverConn,
		addr: serverConn.LocalAddr().String(),
	}

	// Run forwardPacket
	packet := []byte("test packet")
	forwardPacket(packet, destConn)

	// Read from the server
	buffer := make([]byte, 1024)
	serverConn.SetReadDeadline(time.Now().Add(1 * time.Second))
	n, _, err := serverConn.ReadFromUDP(buffer)

	// Assertions
	if err != nil {
		t.Logf("Error reading from UDP: %v", err)
	}
	t.Logf("Bytes read: %d", n)
	t.Logf("Buffer content: %v", buffer[:n])

	assert.NoError(t, err)
	assert.Equal(t, packet, buffer[:n])
}
