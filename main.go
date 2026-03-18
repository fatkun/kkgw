package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultTimeoutMs   = 3000
	defaultWorkers     = 4
	defaultRepeatCount = 2
	repeatInterval     = 1 * time.Second
	defaultCodexModel  = "gpt-5.2-codex"
)

type Config struct {
	URLs        []string
	TimeoutMs   int
	Concurrency int
	RepeatCount int
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

	fmt.Printf("开始延迟测试：每个 URL 间隔 %s 测试 %d 次，最终取平均值。\n", repeatInterval, cfg.RepeatCount)
	results := runLatencyTests(cfg)
	bestURL, hasBest := printResults(results)
	if !hasBest {
		fmt.Fprintln(os.Stderr, "未选出可用域名，跳过 AI 检测。")
		return
	}

	apiKey := ""
	shouldTestAI, err := promptTestAI()
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取 AI 测试确认失败: %v\n", err)
		return
	}
	if shouldTestAI {
		apiKey, err = promptAPIKey()
		if err != nil {
			fmt.Fprintf(os.Stderr, "读取 API Key 失败: %v\n", err)
			return
		}
		if err := checkOpenAIResponses(bestURL, apiKey); err != nil {
			fmt.Fprintf(os.Stderr, "AI 测试失败(%s): %v\n", bestURL, err)
			fmt.Println("已跳过 codex 配置。")
			return
		}
	}

	shouldConfig, err := promptConfigureCodex()
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取配置确认失败: %v\n", err)
		return
	}
	if !shouldConfig {
		fmt.Println("已跳过 codex 配置。")
		return
	}
	if apiKey == "" {
		apiKey, err = promptAPIKey()
		if err != nil {
			fmt.Fprintf(os.Stderr, "读取 API Key 失败: %v\n", err)
			return
		}
	}

	model, err := promptCodexModel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取模型失败: %v\n", err)
		return
	}
	configPath, err := configureCodex(model, apiKey, bestURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "配置 codex 失败: %v\n", err)
		return
	}
	fmt.Println("codex 配置完成。")
	fmt.Printf("配置路径：%s\n", configPath)
}

func waitForExit() {
	fmt.Println()
	fmt.Print("按回车键退出...")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}

func promptTestAI() (bool, error) {
	fmt.Print("是否测试AI对话（Y/n）")
	input, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false, err
	}
	input = strings.TrimSpace(input)
	if input == "" || strings.EqualFold(input, "y") {
		return true, nil
	}
	return false, nil
}

func promptAPIKey() (string, error) {
	fmt.Print("密钥：")
	key, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return "", err
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", errors.New("API Key 不能为空")
	}
	return key, nil
}

func promptConfigureCodex() (bool, error) {
	fmt.Print("是否配置codex(Y/n)")
	input, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false, err
	}
	input = strings.TrimSpace(input)
	if input == "" || strings.EqualFold(input, "y") {
		return true, nil
	}
	return false, nil
}

func promptCodexModel() (string, error) {
	fmt.Printf("使用模型（默认：%s）", defaultCodexModel)
	input, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return "", err
	}
	input = strings.TrimSpace(input)
	if input == "" {
		return defaultCodexModel, nil
	}
	return input, nil
}

func configureCodex(model, apiKey, baseURL string) (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("获取用户目录失败: %w", err)
	}

	codexDir := filepath.Join(homeDir, ".codex")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		return "", fmt.Errorf("创建 codex 目录失败: %w", err)
	}

	timestamp := time.Now().Format("20060102_150405")
	configPath := filepath.Join(codexDir, "config.toml")
	authPath := filepath.Join(codexDir, "auth.json")

	if err := upsertCodexConfigToml(configPath, model, baseURL, timestamp); err != nil {
		return "", err
	}
	if err := upsertCodexAuthJSON(authPath, apiKey, timestamp); err != nil {
		return "", err
	}

	return configPath, nil
}

type tomlAssignment struct {
	key   string
	value string
}

func upsertCodexConfigToml(path, model, baseURL, timestamp string) error {
	existing, existed, err := readFileIfExists(path)
	if err != nil {
		return fmt.Errorf("读取 config.toml 失败: %w", err)
	}

	updated := buildCodexConfigToml(existing, model, baseURL)

	if existed {
		if err := backupFile(path, timestamp); err != nil {
			return fmt.Errorf("备份 config.toml 失败: %w", err)
		}
	}
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return fmt.Errorf("写入 config.toml 失败: %w", err)
	}
	return nil
}

func upsertCodexAuthJSON(path, apiKey, timestamp string) error {
	existing, existed, err := readFileIfExists(path)
	if err != nil {
		return fmt.Errorf("读取 auth.json 失败: %w", err)
	}

	if existed {
		if err := backupFile(path, timestamp); err != nil {
			return fmt.Errorf("备份 auth.json 失败: %w", err)
		}
	}

	content := make(map[string]any)
	if strings.TrimSpace(string(existing)) != "" {
		if err := json.Unmarshal(existing, &content); err != nil {
			return fmt.Errorf("解析 auth.json 失败: %w", err)
		}
	}
	content["OPENAI_API_KEY"] = apiKey

	out, err := json.MarshalIndent(content, "", "  ")
	if err != nil {
		return fmt.Errorf("生成 auth.json 失败: %w", err)
	}
	out = append(out, '\n')
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return fmt.Errorf("写入 auth.json 失败: %w", err)
	}
	return nil
}

func buildCodexConfigToml(existing []byte, model, baseURL string) string {
	topAssignments := []tomlAssignment{
		{key: "model_provider", value: strconv.Quote("custom")},
		{key: "model", value: strconv.Quote(model)},
		{key: "model_reasoning_effort", value: strconv.Quote("high")},
		{key: "disable_response_storage", value: "true"},
	}
	customAssignments := []tomlAssignment{
		{key: "name", value: strconv.Quote("custom")},
		{key: "wire_api", value: strconv.Quote("responses")},
		{key: "requires_openai_auth", value: "true"},
		{key: "base_url", value: strconv.Quote(baseURL)},
	}
	migrationAssignments := []tomlAssignment{
		{key: strconv.Quote("gpt-5.2-codex"), value: strconv.Quote("gpt-5.4")},
	}

	if strings.TrimSpace(string(existing)) == "" {
		return renderNewCodexConfig(topAssignments, customAssignments, migrationAssignments)
	}

	original := strings.ReplaceAll(string(existing), "\r\n", "\n")
	lines := strings.Split(original, "\n")

	topValues := make(map[string]string, len(topAssignments))
	customValues := make(map[string]string, len(customAssignments))
	migrationValues := make(map[string]string, len(migrationAssignments))
	seenTop := make(map[string]bool, len(topAssignments))
	seenCustom := make(map[string]bool, len(customAssignments))
	seenMigration := make(map[string]bool, len(migrationAssignments))

	for _, item := range topAssignments {
		topValues[item.key] = item.value
	}
	for _, item := range customAssignments {
		customValues[item.key] = item.value
	}
	for _, item := range migrationAssignments {
		migrationValues[item.key] = item.value
	}

	out := make([]string, 0, len(lines)+16)
	currentSection := ""
	firstSectionIndex := -1
	customSectionStart := -1
	customSectionEnd := -1
	migrationSectionStart := -1
	migrationSectionEnd := -1

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if section, ok := parseTomlSection(trimmed); ok {
			if firstSectionIndex == -1 {
				firstSectionIndex = len(out)
			}
			if currentSection == "model_providers.custom" && customSectionEnd == -1 {
				customSectionEnd = len(out)
			}
			if currentSection == "notice.model_migrations" && migrationSectionEnd == -1 {
				migrationSectionEnd = len(out)
			}
			currentSection = section
			if section == "model_providers.custom" && customSectionStart == -1 {
				customSectionStart = len(out)
			}
			if section == "notice.model_migrations" && migrationSectionStart == -1 {
				migrationSectionStart = len(out)
			}
			out = append(out, line)
			continue
		}

		if key, ok := parseTomlKey(trimmed); ok {
			if currentSection == "" {
				if value, exists := topValues[key]; exists {
					line = fmt.Sprintf("%s = %s", key, value)
					seenTop[key] = true
				}
			} else if currentSection == "model_providers.custom" {
				if value, exists := customValues[key]; exists {
					line = fmt.Sprintf("%s = %s", key, value)
					seenCustom[key] = true
				}
			} else if currentSection == "notice.model_migrations" {
				if value, exists := migrationValues[key]; exists {
					line = fmt.Sprintf("%s = %s", key, value)
					seenMigration[key] = true
				}
			}
		}
		out = append(out, line)
	}

	if currentSection == "model_providers.custom" && customSectionEnd == -1 {
		customSectionEnd = len(out)
	}
	if currentSection == "notice.model_migrations" && migrationSectionEnd == -1 {
		migrationSectionEnd = len(out)
	}

	missingTop := collectMissingTomlLines(topAssignments, seenTop)
	if len(missingTop) > 0 {
		insertAt := len(out)
		if firstSectionIndex >= 0 {
			insertAt = firstSectionIndex
		}
		out = insertLines(out, insertAt, missingTop)
		if customSectionStart >= insertAt {
			customSectionStart += len(missingTop)
		}
		if customSectionEnd >= insertAt {
			customSectionEnd += len(missingTop)
		}
		if migrationSectionStart >= insertAt {
			migrationSectionStart += len(missingTop)
		}
		if migrationSectionEnd >= insertAt {
			migrationSectionEnd += len(missingTop)
		}
	}

	missingCustom := collectMissingTomlLines(customAssignments, seenCustom)
	if customSectionStart == -1 {
		block := make([]string, 0, len(missingCustom)+2)
		if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) != "" {
			block = append(block, "")
		}
		block = append(block, "[model_providers.custom]")
		block = append(block, missingCustom...)
		out = append(out, block...)
	} else if len(missingCustom) > 0 {
		insertAt := customSectionEnd
		if insertAt == -1 {
			insertAt = len(out)
		}
		out = insertLines(out, insertAt, missingCustom)
		if migrationSectionStart >= insertAt {
			migrationSectionStart += len(missingCustom)
		}
		if migrationSectionEnd >= insertAt {
			migrationSectionEnd += len(missingCustom)
		}
	}

	missingMigration := collectMissingTomlLines(migrationAssignments, seenMigration)
	if migrationSectionStart == -1 {
		block := make([]string, 0, len(missingMigration)+2)
		if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) != "" {
			block = append(block, "")
		}
		block = append(block, "[notice.model_migrations]")
		block = append(block, missingMigration...)
		out = append(out, block...)
	} else if len(missingMigration) > 0 {
		insertAt := migrationSectionEnd
		if insertAt == -1 {
			insertAt = len(out)
		}
		out = insertLines(out, insertAt, missingMigration)
	}

	result := strings.Join(out, "\n")
	if !strings.HasSuffix(result, "\n") {
		result += "\n"
	}
	return result
}

func renderNewCodexConfig(topAssignments, customAssignments, migrationAssignments []tomlAssignment) string {
	lines := make([]string, 0, len(topAssignments)+len(customAssignments)+len(migrationAssignments)+4)
	for _, item := range topAssignments {
		lines = append(lines, fmt.Sprintf("%s = %s", item.key, item.value))
	}
	lines = append(lines, "")
	lines = append(lines, "[model_providers.custom]")
	for _, item := range customAssignments {
		lines = append(lines, fmt.Sprintf("%s = %s", item.key, item.value))
	}
	lines = append(lines, "")
	lines = append(lines, "[notice.model_migrations]")
	for _, item := range migrationAssignments {
		lines = append(lines, fmt.Sprintf("%s = %s", item.key, item.value))
	}
	return strings.Join(lines, "\n") + "\n"
}

func collectMissingTomlLines(assignments []tomlAssignment, seen map[string]bool) []string {
	lines := make([]string, 0, len(assignments))
	for _, item := range assignments {
		if seen[item.key] {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s = %s", item.key, item.value))
	}
	return lines
}

func parseTomlSection(trimmed string) (string, bool) {
	if strings.HasPrefix(trimmed, "[[") && strings.HasSuffix(trimmed, "]]") {
		name := strings.TrimSpace(trimmed[2 : len(trimmed)-2])
		if name == "" {
			return "", false
		}
		return name, true
	}
	if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
		name := strings.TrimSpace(trimmed[1 : len(trimmed)-1])
		if name == "" {
			return "", false
		}
		return name, true
	}
	return "", false
}

func parseTomlKey(trimmed string) (string, bool) {
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", false
	}
	eq := strings.Index(trimmed, "=")
	if eq <= 0 {
		return "", false
	}
	key := strings.TrimSpace(trimmed[:eq])
	if key == "" {
		return "", false
	}
	return key, true
}

func insertLines(lines []string, index int, inserted []string) []string {
	if len(inserted) == 0 {
		return lines
	}
	out := make([]string, 0, len(lines)+len(inserted))
	out = append(out, lines[:index]...)
	out = append(out, inserted...)
	out = append(out, lines[index:]...)
	return out
}

func readFileIfExists(path string) ([]byte, bool, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		return data, true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	return nil, false, err
}

func backupFile(path, timestamp string) error {
	sourceInfo, err := os.Stat(path)
	if err != nil {
		return err
	}

	backupPath := path + ".bak." + timestamp
	for i := 1; ; i++ {
		if _, err := os.Stat(backupPath); errors.Is(err, os.ErrNotExist) {
			break
		} else if err != nil {
			return err
		}
		backupPath = fmt.Sprintf("%s.%d", path+".bak."+timestamp, i)
	}

	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.OpenFile(backupPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, sourceInfo.Mode().Perm())
	if err != nil {
		return err
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	return nil
}

func checkOpenAIResponses(baseURL, apiKey string) error {
	endpoint := strings.TrimRight(baseURL, "/") + "/responses"

	payload := map[string]any{
		"model": "gpt-5.2",
		"input": "hi",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("序列化请求失败: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 35 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("调用 Responses 接口失败: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("读取响应失败: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, parseOpenAIError(raw))
	}

	outputText, err := extractResponseText(raw)
	if err != nil {
		return fmt.Errorf("解析响应内容失败: %w", err)
	}

	fmt.Println("AI 检测成功，返回内容：")
	fmt.Println(outputText)
	return nil
}

func parseOpenAIError(raw []byte) string {
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.Unmarshal(raw, &parsed); err == nil && parsed.Error.Message != "" {
		return parsed.Error.Message
	}

	msg := strings.TrimSpace(string(raw))
	if msg == "" {
		return "未知错误"
	}
	return msg
}

func extractResponseText(raw []byte) (string, error) {
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", err
	}

	if text := collectOutputTextFromTopLevel(parsed); text != "" {
		return text, nil
	}

	outputs, ok := parsed["output"].([]any)
	if !ok {
		return "", errors.New("响应中没有 output 字段")
	}

	texts := make([]string, 0)
	for _, outputItem := range outputs {
		outputMap, ok := outputItem.(map[string]any)
		if !ok {
			continue
		}
		contentItems, ok := outputMap["content"].([]any)
		if !ok {
			continue
		}
		for _, contentItem := range contentItems {
			contentMap, ok := contentItem.(map[string]any)
			if !ok {
				continue
			}
			text, _ := contentMap["text"].(string)
			if text != "" {
				texts = append(texts, text)
			}
		}
	}

	if len(texts) == 0 {
		return "", errors.New("未提取到文本内容")
	}
	return strings.Join(texts, "\n"), nil
}

func collectOutputTextFromTopLevel(parsed map[string]any) string {
	raw, exists := parsed["output_text"]
	if !exists || raw == nil {
		return ""
	}

	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []any:
		lines := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				lines = append(lines, strings.TrimSpace(s))
			}
		}
		return strings.Join(lines, "\n")
	default:
		return ""
	}
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
		RepeatCount: defaultRepeatCount,
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
		case "repeat_count":
			inURLs = false
			n, err := strconv.Atoi(normalizeValue(value))
			if err != nil {
				return Config{}, fmt.Errorf("repeat_count 必须是整数: %w", err)
			}
			if n <= 0 {
				return Config{}, errors.New("repeat_count 必须大于 0")
			}
			cfg.RepeatCount = n
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
	repeatCount := cfg.RepeatCount

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
					lastErr = fmt.Errorf("%d次测试均失败", repeatCount)
				}
				results <- testResult{
					index: idx,
					url:   targetURL,
					err:   fmt.Errorf("%d次测试均失败，最后错误: %w", repeatCount, lastErr),
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

func printResults(results []testResult) (string, bool) {
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
		return "", false
	}
	fmt.Printf("%s\n", bestURL)
	return bestURL, true
}
