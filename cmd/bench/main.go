// bench runs a classification accuracy and latency benchmark against a running
// llm-router instance using the lightweight /v1/classify endpoint (no model
// generation — pure routing measurement).
//
// Each prompt is sent twice: the second run exercises the classification cache.
//
// Usage:
//
//	go run ./cmd/bench [--url http://localhost:8080] [--key llmr_...]
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"text/tabwriter"
	"time"
)

type testCase struct {
	prompt   string
	expected string // thinking | coding | simple | general
}

var cases = []testCase{
	// coding
	{"Write a Python script to scrape a website", "coding"},
	{"Debug this null pointer exception in Java", "coding"},
	{"Implement a red-black tree in C++", "coding"},
	{"Fix the off-by-one error in my binary search", "coding"},
	{"How do I reverse a linked list", "coding"},
	{"Explain why my React component keeps re-rendering", "coding"},
	{"Design a database schema for an e-commerce platform", "coding"},
	{"Refactor this function to use async/await", "coding"},
	{"Write a Go function to reverse a string", "coding"},
	{"Write unit tests for a REST API handler", "coding"},
	// thinking
	{"Plan a 6-month go-to-market strategy for a B2B SaaS", "thinking"},
	{"Analyze the tradeoffs between REST and GraphQL architectures", "thinking"},
	{"Solve this: if all bloops are razzles and all razzles are lazzles, are all bloops lazzles", "thinking"},
	{"What are the architectural implications of moving to a microservices model", "thinking"},
	{"Plan a 3-month roadmap for a new developer tool", "thinking"},
	// simple
	{"What is the capital of France", "simple"},
	{"Hi there", "simple"},
	{"What is 7 times 8", "simple"},
	{"What year did World War 2 end", "simple"},
	{"Yes or no: is Python interpreted", "simple"},
	// general
	{"Translate this paragraph to Spanish: The weather is nice today", "general"},
	{"Summarize the key ideas of stoic philosophy", "general"},
	{"Write a haiku about the ocean", "general"},
	{"Why is the sky blue", "general"},
	{"Write a cover letter for a software engineer job", "general"},
}

type classifyResponse struct {
	Classification string `json:"classification"`
	Model          string `json:"model"`
	CacheHit       bool   `json:"cache_hit"`
	LatencyMS      int64  `json:"latency_ms"`
}

type result struct {
	prompt    string
	expected  string
	got       string
	model     string
	latencyMS int64
	cacheHit  bool
	correct   bool
}

func classify(url, key, prompt string) (classifyResponse, error) {
	body, _ := json.Marshal(map[string]any{
		"model":    "auto",
		"messages": []map[string]string{{"role": "user", "content": prompt}},
	})

	req, _ := http.NewRequest("POST", url+"/v1/classify", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return classifyResponse{}, err
	}
	defer resp.Body.Close()

	b, _ := io.ReadAll(resp.Body)
	var cr classifyResponse
	json.Unmarshal(b, &cr)
	return cr, nil
}

func main() {
	url := flag.String("url", "http://localhost:8080", "Router base URL")
	key := flag.String("key", os.Getenv("LLMR_API_KEY"), "API key (or set LLMR_API_KEY env var)")
	flag.Parse()

	fmt.Printf("Benchmarking %s  (%d cases × 2 runs)\n\n", *url, len(cases))

	var coldResults, warmResults []result

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN\tPROMPT\tEXP\tGOT\tCACHE\tMS\tOK")
	fmt.Fprintln(tw, "---\t------\t---\t---\t-----\t--\t--")

	for _, tc := range cases {
		for run := 0; run < 2; run++ {
			start := time.Now()
			cr, err := classify(*url, *key, tc.prompt)
			wallMS := time.Since(start).Milliseconds()
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				continue
			}

			// Use server-reported latency (classifier only), fall back to wall time
			ms := cr.LatencyMS
			if ms == 0 {
				ms = wallMS
			}

			ok := cr.Classification == tc.expected
			r := result{
				prompt:    tc.prompt,
				expected:  tc.expected,
				got:       cr.Classification,
				model:     cr.Model,
				latencyMS: ms,
				cacheHit:  cr.CacheHit,
				correct:   ok,
			}

			label := "cold"
			if run == 1 {
				label = "warm"
				warmResults = append(warmResults, r)
			} else {
				coldResults = append(coldResults, r)
			}

			mark := "✓"
			if !ok {
				mark = "✗"
			}
			cached := " "
			if cr.CacheHit {
				cached = "⚡"
			}
			fmt.Fprintf(tw, "%s\t%-55s\t%s\t%s\t%s\t%d\t%s\n",
				label, truncate(tc.prompt, 55), tc.expected, cr.Classification, cached, ms, mark)
		}
	}
	tw.Flush()

	fmt.Println()
	printSummary("Cold (classifier called)", coldResults)
	printSummary("Warm (cache hit)        ", warmResults)
	printAccuracyByCategory(coldResults)
	printMisclassified(coldResults)
}

func printSummary(label string, results []result) {
	if len(results) == 0 {
		return
	}
	var correct int
	var totalMS, minMS, maxMS int64
	minMS = 1<<62 - 1
	for _, r := range results {
		if r.correct {
			correct++
		}
		totalMS += r.latencyMS
		if r.latencyMS < minMS {
			minMS = r.latencyMS
		}
		if r.latencyMS > maxMS {
			maxMS = r.latencyMS
		}
	}
	n := int64(len(results))
	fmt.Printf("%s  accuracy=%d/%d (%.0f%%)  avg=%dms  min=%dms  max=%dms\n",
		label, correct, n, float64(correct)/float64(n)*100, totalMS/n, minMS, maxMS)
}

func printAccuracyByCategory(results []result) {
	counts := map[string][2]int64{}
	for _, r := range results {
		c := counts[r.expected]
		c[1]++
		if r.correct {
			c[0]++
		}
		counts[r.expected] = c
	}
	fmt.Println("\nAccuracy by category (cold runs):")
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, cat := range []string{"coding", "thinking", "simple", "general"} {
		c := counts[cat]
		pct := 0.0
		if c[1] > 0 {
			pct = float64(c[0]) / float64(c[1]) * 100
		}
		fmt.Fprintf(tw, "  %-10s\t%d/%d\t(%.0f%%)\n", cat, c[0], c[1], pct)
	}
	tw.Flush()
}

func printMisclassified(results []result) {
	var misses []result
	for _, r := range results {
		if !r.correct {
			misses = append(misses, r)
		}
	}
	if len(misses) == 0 {
		fmt.Println("\nAll cases classified correctly ✓")
		return
	}
	fmt.Printf("\nMisclassified (%d):\n", len(misses))
	for _, r := range misses {
		fmt.Printf("  [%s→%s] %s\n", r.expected, r.got, r.prompt)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
