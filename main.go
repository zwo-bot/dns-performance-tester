package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// QueryResult captures the outcome of a single DNS query.
type QueryResult struct {
	Duration time.Duration
	Success  bool
}

// Config holds the immutable settings shared by all workers.
type Config struct {
	DNSServer  string
	Domain     string
	RecordType uint16
	Protocol   string
	Timeout    time.Duration
	Verbose    bool
}

func performDNSQuery(cfg Config) QueryResult {
	start := time.Now()

	conn, err := net.DialTimeout(cfg.Protocol, cfg.DNSServer, cfg.Timeout)
	if err != nil {
		if cfg.Verbose {
			log.Printf("Error connecting to DNS server: %v", err)
		}
		return QueryResult{time.Since(start), false}
	}
	defer conn.Close()

	// Apply a single deadline that covers the write and the read.
	if err := conn.SetDeadline(time.Now().Add(cfg.Timeout)); err != nil {
		if cfg.Verbose {
			log.Printf("Error setting deadline: %v", err)
		}
		return QueryResult{time.Since(start), false}
	}

	m := new(dnsmessage.Message)
	m.Header.ID = uint16(rand.Intn(65536))
	m.Header.RecursionDesired = true
	m.Questions = []dnsmessage.Question{
		{
			Name:  dnsmessage.MustNewName(cfg.Domain + "."),
			Type:  dnsmessage.Type(cfg.RecordType),
			Class: dnsmessage.ClassINET,
		},
	}

	packed, err := m.Pack()
	if err != nil {
		if cfg.Verbose {
			log.Printf("Error packing DNS message: %v", err)
		}
		return QueryResult{time.Since(start), false}
	}

	resp, err := exchange(conn, cfg.Protocol, packed)
	if err != nil {
		if cfg.Verbose {
			log.Printf("Error during DNS exchange: %v", err)
		}
		return QueryResult{time.Since(start), false}
	}

	var response dnsmessage.Message
	if err := response.Unpack(resp); err != nil {
		if cfg.Verbose {
			log.Printf("Error unpacking DNS response: %v", err)
		}
		return QueryResult{time.Since(start), false}
	}

	// A response is only counted as successful when it matches the query we
	// sent (same transaction ID) and the server reports a success RCODE.
	if response.Header.ID != m.Header.ID {
		if cfg.Verbose {
			log.Printf("Response ID mismatch: got %d, want %d", response.Header.ID, m.Header.ID)
		}
		return QueryResult{time.Since(start), false}
	}
	if response.Header.RCode != dnsmessage.RCodeSuccess {
		if cfg.Verbose {
			log.Printf("Server returned non-success RCODE: %v", response.Header.RCode)
		}
		return QueryResult{time.Since(start), false}
	}

	return QueryResult{time.Since(start), true}
}

// exchange writes a DNS query and reads the response, handling the framing
// differences between UDP (single datagram) and TCP (2-byte length prefix).
func exchange(conn net.Conn, protocol string, query []byte) ([]byte, error) {
	if protocol == "tcp" {
		framed := make([]byte, 2+len(query))
		binary.BigEndian.PutUint16(framed[:2], uint16(len(query)))
		copy(framed[2:], query)
		if _, err := conn.Write(framed); err != nil {
			return nil, err
		}

		lenBuf := make([]byte, 2)
		if _, err := io.ReadFull(conn, lenBuf); err != nil {
			return nil, err
		}
		resp := make([]byte, binary.BigEndian.Uint16(lenBuf))
		if _, err := io.ReadFull(conn, resp); err != nil {
			return nil, err
		}
		return resp, nil
	}

	if _, err := conn.Write(query); err != nil {
		return nil, err
	}
	// 4096 bytes comfortably covers EDNS0-sized UDP responses.
	resp := make([]byte, 4096)
	n, err := conn.Read(resp)
	if err != nil {
		return nil, err
	}
	return resp[:n], nil
}

func worker(ctx context.Context, cfg Config, jobs <-chan int, results chan<- QueryResult, wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-jobs:
			if !ok {
				return
			}
			results <- performDNSQuery(cfg)
		}
	}
}

// Stats holds the aggregated results of a test run.
type Stats struct {
	Domain         string  `json:"domain"`
	RecordType     string  `json:"record_type"`
	DNSServer      string  `json:"dns_server"`
	Protocol       string  `json:"protocol"`
	Concurrency    int     `json:"concurrency"`
	TotalQueries   int64   `json:"total_queries"`
	SuccessQueries int64   `json:"successful_queries"`
	FailedQueries  int64   `json:"failed_queries"`
	SuccessRate    float64 `json:"success_rate_percent"`
	TotalSeconds   float64 `json:"total_seconds"`
	QPS            float64 `json:"queries_per_second"`
	MinMs          float64 `json:"min_ms"`
	AvgMs          float64 `json:"avg_ms"`
	P50Ms          float64 `json:"p50_ms"`
	P95Ms          float64 `json:"p95_ms"`
	P99Ms          float64 `json:"p99_ms"`
	MaxMs          float64 `json:"max_ms"`
}

// percentile returns the value at the given percentile (0-100) from a sorted
// slice of durations, in milliseconds.
func percentile(sorted []time.Duration, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if p <= 0 {
		return float64(sorted[0].Microseconds()) / 1000
	}
	if p >= 100 {
		return float64(sorted[len(sorted)-1].Microseconds()) / 1000
	}
	idx := int(p/100*float64(len(sorted)-1) + 0.5)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return float64(sorted[idx].Microseconds()) / 1000
}

func main() {
	domain := flag.String("domain", "", "Domain name to query")
	recordTypeStr := flag.String("type", "A", "DNS record type (A, AAAA, MX, TXT, NS, CNAME, SOA, PTR)")
	queries := flag.Int("queries", -1, "Number of queries to perform (-1 for continuous)")
	concurrency := flag.Int("concurrency", 10, "Number of concurrent queries")
	dnsServer := flag.String("dns", "8.8.8.8", "DNS server to use (IP or IP:port)")
	protocol := flag.String("protocol", "udp", "Transport protocol to use (udp or tcp)")
	timeout := flag.Duration("timeout", 5*time.Second, "Per-query timeout (e.g. 500ms, 2s)")
	logFile := flag.String("log", "", "Log file to write DNS query errors (default: stderr)")
	verbose := flag.Bool("verbose", false, "Log every individual query error")
	jsonOutput := flag.Bool("json", false, "Output final results as JSON")
	flag.Parse()

	if *domain == "" {
		fmt.Println("Please provide a domain name using the -domain flag")
		os.Exit(1)
	}

	*protocol = strings.ToLower(*protocol)
	if *protocol != "udp" && *protocol != "tcp" {
		fmt.Printf("Invalid protocol: %s (must be udp or tcp)\n", *protocol)
		os.Exit(1)
	}

	if *concurrency < 1 {
		fmt.Println("Concurrency must be at least 1")
		os.Exit(1)
	}

	if *timeout <= 0 {
		fmt.Println("Timeout must be greater than 0")
		os.Exit(1)
	}

	if !strings.Contains(*dnsServer, ":") {
		*dnsServer = *dnsServer + ":53"
	}

	if *logFile != "" {
		f, err := os.OpenFile(*logFile, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
		if err != nil {
			log.Fatalf("error opening file: %v", err)
		}
		defer f.Close()
		log.SetOutput(f)
	}

	recordTypeMap := map[string]uint16{
		"A":     1,
		"NS":    2,
		"CNAME": 5,
		"SOA":   6,
		"PTR":   12,
		"MX":    15,
		"TXT":   16,
		"AAAA":  28,
	}

	recordType, ok := recordTypeMap[strings.ToUpper(*recordTypeStr)]
	if !ok {
		fmt.Printf("Invalid record type: %s\n", *recordTypeStr)
		os.Exit(1)
	}

	cfg := Config{
		DNSServer:  *dnsServer,
		Domain:     *domain,
		RecordType: recordType,
		Protocol:   *protocol,
		Timeout:    *timeout,
		Verbose:    *verbose,
	}

	if !*jsonOutput {
		fmt.Printf("Starting DNS performance test for %s (%s record)\n", *domain, strings.ToUpper(*recordTypeStr))
		fmt.Printf("Using DNS server: %s (%s)\n", *dnsServer, *protocol)
		fmt.Printf("Concurrency: %d, timeout: %s\n", *concurrency, *timeout)

		if *queries != -1 {
			fmt.Printf("Number of queries: %d\n", *queries)
		} else {
			fmt.Println("Running continuously. Press Ctrl+C to stop.")
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	jobs := make(chan int)
	results := make(chan QueryResult, *concurrency)

	var wg sync.WaitGroup
	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go worker(ctx, cfg, jobs, results, &wg)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	start := time.Now()
	var queryCount, successCount int64

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)

	go func() {
		for *queries == -1 || atomic.LoadInt64(&queryCount) < int64(*queries) {
			select {
			case <-ctx.Done():
				return
			case jobs <- 1:
				atomic.AddInt64(&queryCount, 1)
				currentCount := atomic.LoadInt64(&queryCount)
				if !*jsonOutput && (currentCount%10 == 0 || currentCount == int64(*queries)) {
					fmt.Printf("\rCompleted %d queries", currentCount)
					if *queries != -1 {
						fmt.Printf(" (%.1f%%)", float64(currentCount)/float64(*queries)*100)
					}
				}
			}
		}
		close(jobs)
	}()

	var totalDuration time.Duration
	durations := make([]time.Duration, 0, max(*queries, 0))

	done := make(chan struct{})
	go func() {
		for result := range results {
			totalDuration += result.Duration
			durations = append(durations, result.Duration)
			if result.Success {
				atomic.AddInt64(&successCount, 1)
			}
		}
		close(done)
	}()

	select {
	case <-sigChan:
		if !*jsonOutput {
			fmt.Println("\nInterrupted by user. Shutting down...")
		}
		cancel()
	case <-done:
		if !*jsonOutput {
			fmt.Println("\nAll queries completed. Shutting down...")
		}
	}

	<-done // Ensure all results are processed

	finalSuccessCount := atomic.LoadInt64(&successCount)
	finalQueryCount := int64(len(durations))
	elapsed := time.Since(start)

	stats := Stats{
		Domain:         *domain,
		RecordType:     strings.ToUpper(*recordTypeStr),
		DNSServer:      *dnsServer,
		Protocol:       *protocol,
		Concurrency:    *concurrency,
		TotalQueries:   finalQueryCount,
		SuccessQueries: finalSuccessCount,
		FailedQueries:  finalQueryCount - finalSuccessCount,
		TotalSeconds:   elapsed.Seconds(),
	}

	if finalQueryCount > 0 {
		sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
		stats.SuccessRate = float64(finalSuccessCount) / float64(finalQueryCount) * 100
		stats.QPS = float64(finalQueryCount) / elapsed.Seconds()
		stats.AvgMs = float64((totalDuration / time.Duration(finalQueryCount)).Microseconds()) / 1000
		stats.MinMs = percentile(durations, 0)
		stats.P50Ms = percentile(durations, 50)
		stats.P95Ms = percentile(durations, 95)
		stats.P99Ms = percentile(durations, 99)
		stats.MaxMs = percentile(durations, 100)
	}

	if *jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(stats); err != nil {
			log.Fatalf("error encoding JSON: %v", err)
		}
		return
	}

	fmt.Printf("\nResults for %s (%s record):\n", stats.Domain, stats.RecordType)
	fmt.Printf("Total queries: %d\n", stats.TotalQueries)
	fmt.Printf("Successful queries: %d (%.2f%%)\n", stats.SuccessQueries, stats.SuccessRate)
	fmt.Printf("Failed queries: %d (%.2f%%)\n", stats.FailedQueries, 100-stats.SuccessRate)
	fmt.Printf("Total time: %.2f seconds\n", stats.TotalSeconds)
	fmt.Printf("Queries per second: %.2f\n", stats.QPS)
	fmt.Printf("Latency (ms) - min: %.2f, avg: %.2f, p50: %.2f, p95: %.2f, p99: %.2f, max: %.2f\n",
		stats.MinMs, stats.AvgMs, stats.P50Ms, stats.P95Ms, stats.P99Ms, stats.MaxMs)
}
