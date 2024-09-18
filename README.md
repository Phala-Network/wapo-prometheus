# Caddy Log Monitor

This Go application monitors Caddy server logs, tracks IPFS CID requests, and exposes Prometheus metrics.

## Features

- Real-time log file monitoring
- Tracks unique CIDs and their request counts
- Exposes Prometheus metrics
- Provides a health check endpoint

## Usage

1. Build the application:
   go build -o caddy-log-monitor

2. Run the application:
   ./caddy-log-monitor -log /path/to/caddy/access.log -port 8080 -bind 127.0.0.1 -db /path/to/leveldb

## Flags

- -log: Path to the Caddy access log file (required)
- -port: Port to listen on (default: 8080)
- -bind: Address to bind to (default: 127.0.0.1)
- -db: Path to LevelDB database (default: in-memory)
- -h, -help: Show help

## Endpoints

- /metrics: Prometheus metrics
- /health: Health check

## Dependencies

- github.com/fsnotify/fsnotify
- github.com/prometheus/client_golang
- github.com/syndtr/goleveldb

## License

This project is licensed under the Apache License 2.0.
