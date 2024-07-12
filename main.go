package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

type QueryResult struct {
	Duration time.Duration
	Success  bool
}

func performDNSQuery(dnsServer, domain string, recordType uint16) QueryResult {
	start := time.Now()

	conn, err := net.Dial("udp", dnsServer)
	if err != nil {
		log.Printf("Error connecting to DNS server: %v", err)
		return QueryResult{time.Since(start), false}
	}
	defer conn.Close()

	m := new(dnsmessage.Message)
	m.Header.ID = uint16(rand.Intn(65535))
	m.Header.RecursionDesired = true
	m.Questions = []dnsmessage.Question{
		{
			Name:  dnsmessage.MustNewName(domain + "."),
			Type:  dnsmessage.Type(recordType),
			Class: dnsmessage.ClassINET,
		},
	}

	packed, err := m.Pack()
	if err != nil {
		log.Printf("Error packing DNS message: %v", err)
		return QueryResult{time.Since(start), false}
	}

	_, err = conn.Write(packed)
	if err != nil {
		log.Printf("Error sending DNS query: %v", err)
		return QueryResult{time.Since(start), false}
	}

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	resp := make([]byte, 512)
	_, err = conn.Read(resp)
	if err != nil {
		log.Printf("Error reading DNS response: %v", err)
		return QueryResult{time.Since(start), false}
	}

	var response dnsmessage.Message
	err = response.Unpack(resp)
	if err != nil {
		log.Printf("Error unpacking DNS response: %v", err)
		return QueryResult{time.Since(start), false}
	}

	return QueryResult{time.Since(start), true}
}

func worker(ctx context.Context, dnsServer, domain string, recordType uint16, jobs <-chan int, results chan<- QueryResult, wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-jobs:
			if !ok {
				return
			}
			result := performDNSQuery(dnsServer, domain, recordType)
			results <- result
		}
	}
}

func main() {
	domain := flag.String("domain", "", "Domain name to query")
	recordTypeStr := flag.String("type", "A", "DNS record type (A, AAAA, MX, TXT, NS)")
	queries := flag.Int("queries", -1, "Number of queries to perform (-1 for continuous)")
	concurrency := flag.Int("concurrency", 10, "Number of concurrent queries")
	dnsServer := flag.String("dns", "8.8.8.8", "DNS server to use (IP or IP:port)")
	logFile := flag.String("log", "", "Log file to write DNS queries (default: write to stdout)")
	flag.Parse()

	if *domain == "" {
		fmt.Println("Please provide a domain name using the -domain flag")
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

	recordType, ok := recordTypeMap[*recordTypeStr]
	if !ok {
		fmt.Printf("Invalid record type: %s\n", *recordTypeStr)
		os.Exit(1)
	}

	fmt.Printf("Starting DNS performance test for %s (%s record)\n", *domain, *recordTypeStr)
	fmt.Printf("Using DNS server: %s\n", *dnsServer)
	fmt.Printf("Concurrency: %d\n", *concurrency)

	if *queries != -1 {
		fmt.Printf("Number of queries: %d\n", *queries)
	} else {
		fmt.Println("Running continuously. Press Ctrl+C to stop.")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	jobs := make(chan int)
	results := make(chan QueryResult, *concurrency)

	var wg sync.WaitGroup
	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go worker(ctx, *dnsServer, *domain, recordType, jobs, results, &wg)
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
			default:
				jobs <- 1
				atomic.AddInt64(&queryCount, 1)
				currentCount := atomic.LoadInt64(&queryCount)
				if currentCount%10 == 0 || currentCount == int64(*queries) {
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

	done := make(chan struct{})
	go func() {
		for result := range results {
			totalDuration += result.Duration
			if result.Success {
				atomic.AddInt64(&successCount, 1)
			}
		}
		close(done)
	}()

	select {
	case <-sigChan:
		fmt.Println("\nInterrupted by user. Shutting down...")
		cancel()
	case <-done:
		fmt.Println("\nAll queries completed. Shutting down...")
	}

	<-done // Ensure all results are processed

	finalQueryCount := atomic.LoadInt64(&queryCount)
	finalSuccessCount := atomic.LoadInt64(&successCount)
	elapsed := time.Since(start)
	avgDuration := totalDuration / time.Duration(finalQueryCount)
	qps := float64(finalQueryCount) / elapsed.Seconds()
	successRate := float64(finalSuccessCount) / float64(finalQueryCount) * 100

	fmt.Printf("\nResults for %s (%s record):\n", *domain, *recordTypeStr)
	fmt.Printf("Total queries: %d\n", finalQueryCount)
	fmt.Printf("Successful queries: %d (%.2f%%)\n", finalSuccessCount, successRate)
	fmt.Printf("Failed queries: %d (%.2f%%)\n", finalQueryCount-finalSuccessCount, 100-successRate)
	fmt.Printf("Total time: %.2f seconds\n", elapsed.Seconds())
	fmt.Printf("Average query time: %.4f seconds\n", avgDuration.Seconds())
	fmt.Printf("Queries per second: %.2f\n", qps)
}