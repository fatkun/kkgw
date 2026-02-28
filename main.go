package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultTimeoutMs = 3000
	defaultWorkers   = 4
	repeatCount      = 3
	repeatInterval   = 1 * time.Second
)

type Config struct {
	URLs        []string
	TimeoutMs   int
	Concurrency int
}

type testResult struct {
	index   int
	url     string
	status  int
	latency time.Duration
	err     error
}

func main() {
	defer waitForExit()

	cfg, err := loadConfig("config.yaml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取配置失败: %v\n", err)
		return
	}

	fmt.Printf("开始延迟测试：每个 URL 间隔 %s 测试 %d 次，最终取平均值。\n", repeatInterval, repeatCount)
	results := runLatencyTests(cfg)
	printResults(results)
}

func waitForExit() {
	fmt.Println()
	fmt.Print("按回车键退出...")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}

func loadConfig(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer f.Close()

	cfg := Config{
		TimeoutMs:   defaultTimeoutMs,
		Concurrency: defaultWorkers,
	}

	scanner := bufio.NewScanner(f)
	inURLs := false

	for scanner.Scan() {
		line := stripComment(scanner.Text())
		raw := strings.TrimSpace(line)
		if raw == "" {
			continue
		}

		if strings.HasPrefix(raw, "-") {
			if !inURLs {
				return Config{}, fmt.Errorf("无效配置行: %q", raw)
			}
			u := normalizeValue(strings.TrimSpace(strings.TrimPrefix(raw, "-")))
			if u != "" {
				cfg.URLs = append(cfg.URLs, u)
			}
			continue
		}

		key, value, ok := strings.Cut(raw, ":")
		if !ok {
			return Config{}, fmt.Errorf("无效配置行: %q", raw)
		}

		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		switch key {
		case "urls":
			inURLs = true
			if value != "" {
				items, err := parseInlineList(value)
				if err != nil {
					return Config{}, err
				}
				cfg.URLs = append(cfg.URLs, items...)
			}
		case "timeout_ms":
			inURLs = false
			n, err := strconv.Atoi(normalizeValue(value))
			if err != nil {
				return Config{}, fmt.Errorf("timeout_ms 必须是整数: %w", err)
			}
			if n <= 0 {
				return Config{}, errors.New("timeout_ms 必须大于 0")
			}
			cfg.TimeoutMs = n
		case "concurrency":
			inURLs = false
			n, err := strconv.Atoi(normalizeValue(value))
			if err != nil {
				return Config{}, fmt.Errorf("concurrency 必须是整数: %w", err)
			}
			if n <= 0 {
				return Config{}, errors.New("concurrency 必须大于 0")
			}
			cfg.Concurrency = n
		default:
			inURLs = false
		}
	}

	if err := scanner.Err(); err != nil {
		return Config{}, err
	}
	if len(cfg.URLs) == 0 {
		return Config{}, errors.New("urls 不能为空")
	}
	if cfg.Concurrency > len(cfg.URLs) {
		cfg.Concurrency = len(cfg.URLs)
	}
	return cfg, nil
}

func parseInlineList(value string) ([]string, error) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "[") || !strings.HasSuffix(value, "]") {
		return nil, fmt.Errorf("urls 行格式错误: %q", value)
	}
	body := strings.TrimSpace(value[1 : len(value)-1])
	if body == "" {
		return nil, nil
	}

	parts := strings.Split(body, ",")
	items := make([]string, 0, len(parts))
	for _, p := range parts {
		s := normalizeValue(strings.TrimSpace(p))
		if s != "" {
			items = append(items, s)
		}
	}
	return items, nil
}

func stripComment(s string) string {
	if idx := strings.Index(s, "#"); idx >= 0 {
		return s[:idx]
	}
	return s
}

func normalizeValue(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "\"")
	s = strings.Trim(s, "'")
	return strings.TrimSpace(s)
}

func runLatencyTests(cfg Config) []testResult {
	client := &http.Client{}
	timeout := time.Duration(cfg.TimeoutMs) * time.Millisecond

	jobs := make(chan int)
	results := make(chan testResult, len(cfg.URLs))
	var wg sync.WaitGroup
	var logMu sync.Mutex

	logf := func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		fmt.Printf(format+"\n", args...)
	}
	connIPText := func(ip string, fallback string) string {
		if ip == "" {
			return fallback
		}
		return ip
	}

	worker := func() {
		defer wg.Done()
		for idx := range jobs {
			targetURL := cfg.URLs[idx]

			total := time.Duration(0)
			lastStatus := 0
			successCount := 0
			failCount := 0
			var lastErr error

			for round := 1; round <= repeatCount; round++ {
				ctx, cancel := context.WithTimeout(context.Background(), timeout)
				connIP := ""
				trace := &httptrace.ClientTrace{
					GotConn: func(info httptrace.GotConnInfo) {
						remoteAddr := info.Conn.RemoteAddr().String()
						if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
							connIP = host
							return
						}
						connIP = remoteAddr
					},
				}
				ctx = httptrace.WithClientTrace(ctx, trace)
				// logf("[TEST] %s round=%d/%d start conn_ip=%s", targetURL, round, repeatCount, connIPText(connIP, "(pending)"))

				start := time.Now()
				req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
				if reqErr != nil {
					cancel()
					lastErr = fmt.Errorf("第%d次构建请求失败: %w", round, reqErr)
					failCount++
					logf("[TEST] %s round=%d/%d fail conn_ip=%s err=%v", targetURL, round, repeatCount, connIPText(connIP, "(unknown)"), lastErr)
					// if round < repeatCount {
					// 	time.Sleep(repeatInterval)
					// }
					continue
				}

				resp, err := client.Do(req)
				latency := time.Since(start)
				if err != nil {
					cancel()
					lastErr = fmt.Errorf("第%d次请求失败: %w", round, err)
					failCount++
					logf("[TEST] %s round=%d/%d fail latency=%s conn_ip=%s err=%v", targetURL, round, repeatCount, latency.Round(time.Millisecond), connIPText(connIP, "(unknown)"), lastErr)
					if round < repeatCount {
						time.Sleep(repeatInterval)
					}
					continue
				}

				// 读一个字节即可，避免下载大响应体。
				_, _ = io.CopyN(io.Discard, resp.Body, 1)
				_ = resp.Body.Close()
				cancel()

				lastStatus = resp.StatusCode
				total += latency
				successCount++
				logf("[TEST] %s round=%d/%d done status=%d latency=%s conn_ip=%s", targetURL, round, repeatCount, resp.StatusCode, latency.Round(time.Millisecond), connIPText(connIP, "(unknown)"))

				if round < repeatCount {
					time.Sleep(repeatInterval)
				}
			}

			if successCount == 0 {
				if lastErr == nil {
					lastErr = errors.New("3次测试均失败")
				}
				results <- testResult{
					index: idx,
					url:   targetURL,
					err:   fmt.Errorf("%d次测试均失败，最后错误: %w", failCount, lastErr),
				}
				continue
			}

			results <- testResult{
				index:   idx,
				url:     targetURL,
				status:  lastStatus,
				latency: total / time.Duration(successCount),
			}
		}
	}

	wg.Add(cfg.Concurrency)
	for i := 0; i < cfg.Concurrency; i++ {
		go worker()
	}

	for i := range cfg.URLs {
		jobs <- i
	}
	close(jobs)

	wg.Wait()
	close(results)

	out := make([]testResult, len(cfg.URLs))
	for r := range results {
		out[r.index] = r
	}
	return out
}

func printResults(results []testResult) {
	var (
		bestURL     string
		bestLatency time.Duration
		hasBest     bool
	)

	fmt.Println()
	fmt.Println("延迟测试结果（平均值）:")
	for _, r := range results {
		if r.err != nil {
			fmt.Printf("[FAIL] %s err=%v\n", r.url, r.err)
			continue
		}

		ok := r.status == http.StatusOK || r.status == http.StatusNotFound
		if ok && (!hasBest || r.latency < bestLatency) {
			hasBest = true
			bestURL = r.url
			bestLatency = r.latency
		}

		flag := "FAIL"
		if ok {
			flag = "OK"
		}
		fmt.Printf("[%s] %s status=%d avg_latency=%s\n", flag, r.url, r.status, r.latency.Round(time.Millisecond))
	}

	fmt.Println()
	fmt.Println("推荐使用地址:")
	if !hasBest {
		fmt.Println("(无)")
		return
	}
	fmt.Printf("%s\n", bestURL)
}
