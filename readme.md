# DNS Performance Tester

This Go program is a command-line tool for testing DNS server performance. It allows you to send multiple DNS queries concurrently and measure various performance metrics.

## Features

- Supports multiple DNS record types (A, AAAA, MX, TXT, NS, CNAME, SOA, PTR)
- Configurable number of queries and concurrency level
- UDP and TCP transport support
- Configurable per-query timeout
- Validates responses (matches transaction ID and checks for a success RCODE),
  so SERVFAIL/NXDOMAIN answers are correctly counted as failures
- Custom DNS server support
- Detailed performance statistics, including latency percentiles (min, avg,
  p50, p95, p99, max)
- Optional JSON output for scripting and automation

## Installation

1. Ensure you have Go installed on your system.
2. Clone this repository:
   ```
   git clone https://github.com/zwo-bot/dns-performance-tester.git
   ```
3. Navigate to the project directory:
   ```
   cd dns-performance-tester
   ```
4. Build the program:
   ```
   go build
   ```

## Usage

Run the program with the following command:

```
./dns-performance-tester [flags]
```

### Flags

- `-domain`: Domain name to query (required)
- `-type`: DNS record type (A, AAAA, MX, TXT, NS, CNAME, SOA, PTR) (default "A")
- `-queries`: Number of queries to perform (-1 for continuous) (default -1)
- `-concurrency`: Number of concurrent queries (default 10)
- `-dns`: DNS server to use (IP or IP:port) (default "8.8.8.8")
- `-protocol`: Transport protocol, `udp` or `tcp` (default "udp")
- `-timeout`: Per-query timeout, e.g. `500ms`, `2s` (default 5s)
- `-log`: Log file to write DNS query errors (default: stderr)
- `-verbose`: Log every individual query error (default false)
- `-json`: Output final results as JSON (default false)

### Examples

1. Perform 100 A record queries for example.com using Google's DNS server:
   ```
   ./dns-performance-tester -domain example.com -type A -queries 100
   ```

2. Continuously query MX records for gmail.com with 20 concurrent queries:
   ```
   ./dns-performance-tester -domain gmail.com -type MX -concurrency 20
   ```

3. Run 500 queries over TCP with a 1 second timeout and JSON output:
   ```
   ./dns-performance-tester -domain example.com -queries 500 -protocol tcp -timeout 1s -json
   ```

## Output

The program provides real-time progress updates and final performance statistics, including:

- Total number of queries
- Number and percentage of successful queries
- Number and percentage of failed queries
- Total execution time
- Queries per second
- Latency distribution in milliseconds (min, avg, p50, p95, p99, max)

With `-json`, the same statistics are emitted as a single JSON object instead of
the human-readable summary, which is convenient for piping into other tools.

## Testing

Run the unit tests with:

```
go test ./...
```

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.
