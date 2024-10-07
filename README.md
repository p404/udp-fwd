# UDP Forwarder

This is a simple UDP packet forwarder written in Go. It listens for UDP packets on a specified port and forwards them to one or more destination addresses.

## Features

- UDP packet forwarding
- Multiple destination support
- Prometheus metrics

## Environment Variables

The following environment variables are required to run the program:

- `UDP_LISTEN_PORT`: The port on which the server will listen for incoming UDP packets.
- `UDP_DESTINATIONS`: A comma-separated list of destination addresses (IP:port) to forward packets to.
- `LOG_LEVEL`: (Optional) The logging level. Defaults to "info" if not set. Options are: trace, debug, info, warn, error, fatal, panic.

Example:
```bash
export UDP_LISTEN_PORT=5000
export UDP_DESTINATIONS=10.0.0.1:6000,10.0.0.2:6000
export LOG_LEVEL=debug
```

## Building
To build the program for your current platform:

```bash
go build -o udp-forwarder