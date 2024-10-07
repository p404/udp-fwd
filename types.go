package main

import (
	"net"
	"time"
)

type UDPConn interface {
	ReadFromUDP(b []byte) (int, *net.UDPAddr, error)
	Close() error
	SetReadDeadline(t time.Time) error
}
