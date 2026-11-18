package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Event struct {
	Type string `json:"type"`
	Data any    `json:"data,omitempty"`
}

type LogEntry struct {
	ID    int64  `json:"id"`
	Time  string `json:"time"`
	Level string `json:"level"`
	Msg   string `json:"msg"`
}

type logHub struct {
	mu   sync.Mutex
	seq  int64
	max  int
	logs []LogEntry
	subs map[chan Event]struct{}
}

var hub = &logHub{max: 5000, subs: make(map[chan Event]struct{})}

func (h *logHub) setMax(n int) {
	if n < 100 {
		n = 100
	}
	h.mu.Lock()
	h.max = n
	if len(h.logs) > n {
		h.logs = append([]LogEntry(nil), h.logs[len(h.logs)-n:]...)
	}
	h.mu.Unlock()
}

func (h *logHub) publish(ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (h *logHub) add(level, msg string) {
	msg = strings.TrimRight(msg, "\r\n")
	if strings.TrimSpace(msg) == "" {
		return
	}
	for _, line := range strings.Split(msg, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		h.mu.Lock()
		h.seq++
		e := LogEntry{ID: h.seq, Time: time.Now().Format("2006-01-02 15:04:05"), Level: level, Msg: line}
		h.logs = append(h.logs, e)
		if len(h.logs) > h.max {
			h.logs = h.logs[len(h.logs)-h.max:]
		}
		h.mu.Unlock()
		fmt.Fprintf(os.Stdout, "[%s] %s\n", e.Time, line)
		h.publish(Event{Type: "log", Data: e})
	}
}

func (h *logHub) since(since int64, limit int) []LogEntry {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]LogEntry, 0, 256)
	for _, e := range h.logs {
		if e.ID > since {
			out = append(out, e)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

func (h *logHub) clear() {
	h.mu.Lock()
	h.logs = nil
	h.mu.Unlock()
	h.publish(Event{Type: "logs_cleared"})
}

func (h *logHub) subscribe() chan Event {
	ch := make(chan Event, 512)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *logHub) unsubscribe(ch chan Event) {
	h.mu.Lock()
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch)
	}
	h.mu.Unlock()
}

func logf(format string, a ...any)      { hub.add("info", fmt.Sprintf(format, a...)) }
func logOkf(format string, a ...any)    { hub.add("ok", fmt.Sprintf(format, a...)) }
func logWarnf(format string, a ...any)  { hub.add("warn", fmt.Sprintf(format, a...)) }
func logErrf(format string, a ...any)   { hub.add("error", fmt.Sprintf(format, a...)) }
func logDebugf(format string, a ...any) { hub.add("debug", fmt.Sprintf(format, a...)) }

type Progress struct {
	Phase   string  `json:"phase"`
	Label   string  `json:"label"`
	Total   int     `json:"total"`
	Done    int     `json:"done"`
	Percent float64 `json:"percent"`
}

type RunStats struct {
	Sources     int      `json:"sources"`
	TotalIPs    int      `json:"total_ips"`
	Valid       int      `json:"valid"`
	SpeedTested int      `json:"speed_tested"`
	Qualified   int      `json:"qualified"`
	ResultRows  int      `json:"result_rows"`
	BestSpeed   float64  `json:"best_speed"`
	BestLatency int64    `json:"best_latency"`
	SubFiles    []string `json:"sub_files"`
	DNSPushed   int      `json:"dns_pushed"`
	DNSFailed   int      `json:"dns_failed"`
	OutFile     string   `json:"outfile"`
}

type JobView struct {
	ID         string   `json:"id"`
	Mode       string   `json:"mode"`
	Running    bool     `json:"running"`
	Phase      string   `json:"phase"`
	PhaseLabel string   `json:"phase_label"`
	Error      string   `json:"error"`
	Canceled   bool     `json:"canceled"`
	StartedAt  string   `json:"started_at"`
	EndedAt    string   `json:"ended_at"`
	ElapsedSec float64  `json:"elapsed_sec"`
	Progress   Progress `json:"progress"`
	Stats      RunStats `json:"stats"`
	Settings   Settings `json:"settings"`
}

type jobState struct {
	ID       string
	Mode     string
	Running  bool
	Phase    string
	Label    string
	Error    string
	Canceled bool
	Start    time.Time
	End      time.Time
	Progress Progress
	Stats    RunStats
	Settings Settings
}

var (
	jobMu     sync.Mutex
	js        jobState
	jobCancel context.CancelFunc
	jobDone   chan struct{}
	lastPush  time.Time
)

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02 15:04:05")
}

func snapshotLocked() JobView {
	elapsed := 0.0
	if !js.Start.IsZero() {
		end := js.End
		if js.Running || end.IsZero() {
			end = time.Now()
		}
		elapsed = float64(int64(end.Sub(js.Start).Seconds()*10)) / 10
	}
	return JobView{
		ID: js.ID, Mode: js.Mode, Running: js.Running,
		Phase: js.Phase, PhaseLabel: js.Label, Error: js.Error, Canceled: js.Canceled,
		StartedAt: fmtTime(js.Start), EndedAt: fmtTime(js.End), ElapsedSec: elapsed,
		Progress: js.Progress, Stats: js.Stats, Settings: js.Settings,
	}
}

func jobSnapshot() JobView {
	jobMu.Lock()
	defer jobMu.Unlock()
	return snapshotLocked()
}

func isRunning() bool {
	jobMu.Lock()
	defer jobMu.Unlock()
	return js.Running
}

func startJob(mode string, cfg *AppConfig) (string, error) {
	jobMu.Lock()
	if js.Running {
		jobMu.Unlock()
		return "", errors.New("已有任务正在运行, 请先停止或等待完成")
	}
	ctx, cancel := context.WithCancel(context.Background())
	jobCancel = cancel
	jobDone = make(chan struct{})
	done := jobDone
	id := fmt.Sprintf("job-%d", time.Now().UnixNano())
	st := settingsFromConfig(cfg)
	js = jobState{
		ID: id, Mode: mode, Running: true, Phase: "init", Label: "初始化",
		Start: time.Now(), Settings: st,
		Progress: Progress{Phase: "init", Label: "初始化"},
		Stats:    RunStats{OutFile: st.OutFile},
	}
	jobMu.Unlock()
	broadcastState()
	go func() {
		defer close(done)
		err := runPipeline(ctx, mode, cfg)
		finishJob(ctx, err)
	}()
	return id, nil
}

func finishJob(ctx context.Context, err error) {
	jobMu.Lock()
	js.Running = false
	js.End = time.Now()
	jobCancel = nil
	switch {
	case err != nil && (errors.Is(err, context.Canceled) || ctx.Err() != nil):
		js.Canceled = true
		js.Phase, js.Label, js.Error = "stopped", "已停止", ""
	case err != nil:
		js.Phase, js.Label, js.Error = "error", "失败", err.Error()
	default:
		js.Phase, js.Label, js.Error = "done", "完成", ""
	}
	js.Progress.Phase, js.Progress.Label = js.Phase, js.Label
	if js.Progress.Total > 0 {
		js.Progress.Done = js.Progress.Total
		js.Progress.Percent = 100
	}
	elapsed := js.End.Sub(js.Start).Seconds()
	phase, msg := js.Phase, js.Error
	jobMu.Unlock()

	switch phase {
	case "stopped":
		logWarnf("========== 任务已停止 (耗时 %.1fs) ==========", elapsed)
	case "error":
		logErrf("========== 任务失败: %s (耗时 %.1fs) ==========", msg, elapsed)
	default:
		logOkf("========== 任务完成 (耗时 %.1fs) ==========", elapsed)
	}
	broadcastState()
}

func stopJob() bool {
	jobMu.Lock()
	cancel, running := jobCancel, js.Running
	jobMu.Unlock()
	if !running || cancel == nil {
		return false
	}
	logWarnf("[job] 收到停止请求, 正在取消任务...")
	cancel()
	return true
}

func waitJob() {
	jobMu.Lock()
	ch := jobDone
	jobMu.Unlock()
	if ch != nil {
		<-ch
	}
}

func setPhase(phase, label string, total int) {
	jobMu.Lock()
	js.Phase, js.Label = phase, label
	js.Progress = Progress{Phase: phase, Label: label, Total: total}
	jobMu.Unlock()
	broadcastState()
}

func setProgress(done, total int) {
	jobMu.Lock()
	js.Progress.Done = done
	if total > 0 {
		js.Progress.Total = total
	}
	t := js.Progress.Total
	if t > 0 {
		js.Progress.Percent = float64(done) / float64(t) * 100
	}
	p := js.Progress
	push := time.Since(lastPush) > 120*time.Millisecond || (t > 0 && done >= t)
	if push {
		lastPush = time.Now()
	}
	jobMu.Unlock()
	if push {
		hub.publish(Event{Type: "progress", Data: p})
	}
}

func updateStats(fn func(*RunStats)) {
	jobMu.Lock()
	fn(&js.Stats)
	jobMu.Unlock()
}

func broadcastState() {
	hub.publish(Event{Type: "state", Data: buildState()})
}

func modeLabel(mode string) string {
	switch mode {
	case "full":
		return "完整流程(测速+订阅+DNS)"
	case "speedtest":
		return "仅测速"
	case "sub":
		return "仅生成订阅"
	case "dns":
		return "仅推送 DNS"
	}
	return mode
}

func runPipeline(ctx context.Context, mode string, cfg *AppConfig) error {
	st := settingsFromConfig(cfg)
	logf("========== 任务开始: %s ==========", modeLabel(mode))
	logf("[cfg] 输入源=%d 并发=%d 测速协程=%d 延迟阈值=%dms 轮数=%d 合格速度=%.2fMB/s 合格数量=%d 递归深度=%d",
		len(st.Sources), st.Max, st.SpeedTest, st.Delay, st.Rounds, st.MinSpeed, st.MinSpeedCount, st.SourceDepth)
	logf("[cfg] 结果=%s 归档=%v(%s) 订阅=%v DNS=%v", st.OutFile, st.HistoryEnable, st.History, cfg.SubConfig.Enabled, cfg.DNSConfig.Enabled)
	for i, s := range st.Sources {
		kind := "远程"
		if isLocalSource(s) {
			kind = "本地"
		}
		logf("[cfg] 源 %d [%s] %s", i+1, kind, s)
	}
	initProxy(st.Proxy)

	switch mode {
	case "sub":
		return doSub(cfg, st)
	case "dns":
		return doDNS(ctx, cfg, st)
	}

	setPhase("source", "加载输入源", 0)
	ips, err := fetchIPsFromSources(ctx, st, st.Sources, st.SourceDepth)
	if err != nil {
		return err
	}
	updateStats(func(s *RunStats) { s.TotalIPs = len(ips) })
	if len(ips) == 0 {
		return errors.New("未从输入源中提取到有效 IP")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	sum, err := runSpeedtest(ctx, st, ips)
	if sum != nil {
		updateStats(func(s *RunStats) {
			s.Valid, s.SpeedTested, s.Qualified = sum.Valid, sum.SpeedTested, sum.Qualified
			s.ResultRows, s.BestSpeed, s.BestLatency = sum.Rows, sum.BestSpeed, sum.BestLatency
		})
	}
	if err != nil {
		return err
	}
	if mode == "full" {
		if cfg.SubConfig.Enabled {
			if e := doSub(cfg, st); e != nil {
				logErrf("[sub] %v", e)
			}
		} else {
			logf("[sub] 未开启, 跳过")
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if cfg.DNSConfig.Enabled {
			if e := doDNS(ctx, cfg, st); e != nil {
				logErrf("[dns] %v", e)
			}
		} else {
			logf("[dns] 未开启, 跳过")
		}
	}
	return ctx.Err()
}

func doSub(cfg *AppConfig, st Settings) error {
	setPhase("sub", "生成订阅", 0)
	files, err := GenerateAllSubscriptions(SubCreateConfig{OutFile: st.OutFile, Sub: cfg.SubConfig})
	var names []string
	for _, f := range files {
		if f.Written {
			names = append(names, f.Path)
		}
	}
	updateStats(func(s *RunStats) { s.SubFiles = names })
	return err
}

func doDNS(ctx context.Context, cfg *AppConfig, st Settings) error {
	setPhase("dns", "推送 DNS", 0)
	results, err := loadSpeedResultsFromFile(st.OutFile)
	if err != nil {
		return err
	}
	pushed, failed, err := PushToCloudflareDNS(ctx, results, cfg.DNSConfig)
	updateStats(func(s *RunStats) { s.DNSPushed, s.DNSFailed = pushed, failed })
	return err
}

var (
	serverStart  = time.Now()
	activeHost   string
	activePort   int
	activePrefix string
)

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func errJSON(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"ok": false, "error": msg})
}

func readJSON(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("请求体 JSON 解析失败: %v", err)
	}
	return nil
}

type ProxyInfo struct {
	Configured string `json:"configured"`
	Active     bool   `json:"active"`
}

type MetaInfo struct {
	Version   string  `json:"version"`
	GoVersion string  `json:"go_version"`
	GOOS      string  `json:"goos"`
	GOARCH    string  `json:"goarch"`
	PID       int     `json:"pid"`
	CWD       string  `json:"cwd"`
	StartedAt string  `json:"started_at"`
	UptimeSec float64 `json:"uptime_sec"`
}

type WebActiveInfo struct {
	Host       string `json:"host"`
	Port       int    `json:"port"`
	PathPrefix string `json:"path_prefix"`
}

type StateResp struct {
	Job        JobView       `json:"job"`
	Proxy      ProxyInfo     `json:"proxy"`
	Meta       MetaInfo      `json:"meta"`
	ConfigPath string        `json:"config_path"`
	Web        WebConfig     `json:"web"`
	WebActive  WebActiveInfo `json:"web_active"`
	Summary    struct {
		Sources       int    `json:"sources"`
		OutFile       string `json:"outfile"`
		History       string `json:"history"`
		HistoryEnable bool   `json:"history_enable"`
		SubEnabled    bool   `json:"sub_enabled"`
		SubDir        string `json:"sub_dir"`
		DNSEnabled    bool   `json:"dns_enabled"`
		ResultExists  bool   `json:"result_exists"`
		ResultSize    int64  `json:"result_size"`
		ResultMTime   string `json:"result_mtime"`
	} `json:"summary"`
}

func buildState() StateResp {
	cfg := currentConfig()
	var s StateResp
	s.Job = jobSnapshot()
	raw, active := proxyStatus()
	s.Proxy = ProxyInfo{Configured: raw, Active: active}
	cwd, _ := os.Getwd()
	s.Meta = MetaInfo{
		Version: appVersion, GoVersion: runtime.Version(), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		PID: os.Getpid(), CWD: cwd, StartedAt: fmtTime(serverStart),
		UptimeSec: float64(int64(time.Since(serverStart).Seconds())),
	}
	s.ConfigPath = configPath()
	s.Web = cfg.WebConfig
	s.WebActive = WebActiveInfo{Host: activeHost, Port: activePort, PathPrefix: activePrefix}
	s.Summary.Sources = len(cfg.File)
	s.Summary.OutFile = cfg.OutFile
	s.Summary.History = cfg.History
	s.Summary.HistoryEnable = cfg.HistoryEnable
	s.Summary.SubEnabled = cfg.SubConfig.Enabled
	s.Summary.SubDir = cfg.SubConfig.OutputDir
	s.Summary.DNSEnabled = cfg.DNSConfig.Enabled
	if st, err := os.Stat(cfg.OutFile); err == nil {
		s.Summary.ResultExists = true
		s.Summary.ResultSize = st.Size()
		s.Summary.ResultMTime = st.ModTime().Format("2006-01-02 15:04:05")
	}
	return s
}

func subOutputRoot() string {
	cfg := currentConfig()
	dir := strings.TrimSpace(cfg.SubConfig.OutputDir)
	if dir == "" {
		dir = "html/"
	}
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

func underRoot(root, p string) (string, bool) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return abs, true
}

func resultAllowed(p string) (string, bool) {
	cfg := currentConfig()
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", false
	}
	for _, f := range []string{cfg.OutFile, cfg.History} {
		if strings.TrimSpace(f) == "" {
			continue
		}
		if a, e := filepath.Abs(f); e == nil && a == abs {
			return abs, true
		}
	}
	return underRoot(subOutputRoot(), abs)
}

func serveDownload(w http.ResponseWriter, path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		errJSON(w, 404, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(path)+`"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func readCSVTable(path string) ([]string, [][]string, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, false, err
	}
	defer f.Close()
	rd := csv.NewReader(f)
	rd.FieldsPerRecord = -1
	rd.Comment = '#'
	rd.LazyQuotes = true
	recs, err := rd.ReadAll()
	if err != nil {
		return nil, nil, false, fmt.Errorf("解析 CSV 失败: %v", err)
	}
	var header []string
	var rows [][]string
	hasHeader := false
	for _, r := range recs {
		if len(r) == 0 {
			continue
		}
		first := strings.TrimSpace(strings.TrimPrefix(r[0], "\ufeff"))
		if first == "" {
			continue
		}
		if !hasHeader && len(rows) == 0 && net.ParseIP(first) == nil {
			header = r
			hasHeader = true
			continue
		}
		rows = append(rows, r)
	}
	if !hasHeader {
		header = csvHeader
	}
	return header, rows, hasHeader, nil
}

const decoyBody = "Are you ok?"
func writeDecoy(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, decoyBody)
}

var routeTable = map[string]http.HandlerFunc{
	"/api/state":           handleState,
	"/api/config":          handleConfig,
	"/api/config/reset":    handleConfigReset,
	"/api/run":             handleRun,
	"/api/stop":            handleStop,
	"/api/logs":            handleLogs,
	"/api/events":          handleEvents,
	"/api/results":         handleResults,
	"/api/history":         handleHistory,
	"/api/history/trim":    handleHistoryTrim,
	"/api/history/clear":   handleHistoryClear,
	"/api/files":           handleFiles,
	"/api/file":            handleFile,
	"/api/sources/preview": handleSourcesPreview,
	"/api/proxy/test":      handleProxyTest,
	"/api/ip/test":         handleIPTest,
	"/api/sub/preview":     handleSubPreview,
	"/api/sub/generate":    handleSubGenerate,
	"/api/dns/plan":        handleDNSPlan,
	"/api/dns/records":     handleDNSRecords,
}

func buildRouter() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "" {
			p = "/"
		}
		switch {
		case p == "/":
			guarded(handleIndex)(w, r)
		case strings.HasPrefix(p, "/sub/"):
			guarded(handleSubFile)(w, r)
		default:
			if h, ok := routeTable[p]; ok {
				guarded(h)(w, r)
				return
			}
			writeDecoy(w)
		}
	})
}

func prefixWrap(next http.Handler, prefix string) http.Handler {
	if prefix == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		var inner string
		switch {
		case p == prefix:
			inner = "/"
		case strings.HasPrefix(p, prefix+"/"):
			inner = p[len(prefix):]
		default:
			writeDecoy(w)
			return
		}
		r2 := new(http.Request)
		*r2 = *r
		u2 := *r.URL
		u2.Path = inner
		u2.RawPath = ""
		r2.URL = &u2
		next.ServeHTTP(w, r2)
	})
}

func runWebServer(cfg *AppConfig) error {
	setGlobalConfig(cfg)
	hub.setMax(cfg.WebConfig.LogBuffer)
	activeHost, activePort, activePrefix = cfg.WebConfig.Host, cfg.WebConfig.Port, cfg.WebConfig.PathPrefix
	addr := net.JoinHostPort(cfg.WebConfig.Host, strconv.Itoa(cfg.WebConfig.Port))

	handler := withRecover(prefixWrap(buildRouter(), activePrefix))
	srv := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt)
		<-sig
		logWarnf("[web] 收到中断信号, 正在关闭服务...")
		stopJob()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	go initProxy(cfg.Proxy)

	dhost := cfg.WebConfig.Host
	if dhost == "0.0.0.0" || dhost == "::" || dhost == "" {
		dhost = "127.0.0.1"
	}
	logOkf("[web] IPtest 控制台已启动: http://%s%s/", net.JoinHostPort(dhost, strconv.Itoa(cfg.WebConfig.Port)), activePrefix)
	if activePrefix != "" {
		logf("[web] 路径前缀: %s (所有页面/接口/订阅均在此前缀下, 便于 nginx 反向代理)", activePrefix)
	}
	if cfg.WebConfig.Password != "" {
		logf("[web] 已启用 Basic Auth, 用户名: %s", cfg.WebConfig.Username)
	} else {
		logWarnf("[web] 未设置访问密码, 建议仅监听 127.0.0.1 或配置 web_config.password")
	}
	logf("[web] CLI 模式: -cli / -sub / -dns / -web=false")

	if cfg.WebConfig.AutoRun {
		go func() {
			time.Sleep(800 * time.Millisecond)
			if _, err := startJob("full", currentConfig()); err != nil {
				logWarnf("[web] 自动运行失败: %v", err)
			}
		}()
	}
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	logf("[web] 服务已关闭")
	return nil
}

func withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if e := recover(); e != nil {
				logErrf("[web] 处理 %s 异常: %v", r.URL.Path, e)
				errJSON(w, 500, fmt.Sprintf("内部错误: %v", e))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func checkAuth(w http.ResponseWriter, r *http.Request) bool {
	cfg := currentConfig()
	if cfg.WebConfig.Password == "" {
		return true
	}
	u, p, ok := r.BasicAuth()
	user := cfg.WebConfig.Username
	if user == "" {
		user = "iptest"
	}
	if !ok || u != user || p != cfg.WebConfig.Password {
		w.Header().Set("WWW-Authenticate", `Basic realm="iptest", charset="UTF-8"`)
		errJSON(w, 401, "未授权")
		return false
	}
	return true
}

func guarded(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) {
			return
		}
		h(w, r)
	}
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	page := strings.ReplaceAll(indexHTML, "__BASE__", activePrefix)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, page)
}

func handleSubFile(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/sub/")
	if name == "" || strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	p := filepath.Join(subOutputRoot(), filepath.Base(name))
	data, err := os.ReadFile(p)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func handleState(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, buildState()) }

func handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if r.URL.Query().Get("reload") == "1" {
			cfg, err := readConfigFile(configPath())
			if err != nil {
				errJSON(w, 500, "重新载入失败: "+err.Error())
				return
			}
			setGlobalConfig(cfg)
			hub.setMax(cfg.WebConfig.LogBuffer)
			logOkf("[cfg] 已从文件重新载入: %s", configPath())
		}
		writeJSON(w, 200, map[string]any{"ok": true, "path": configPath(), "config": currentConfig()})
	case http.MethodPost, http.MethodPut:
		var cfg AppConfig
		if err := readJSON(r, &cfg); err != nil {
			errJSON(w, 400, err.Error())
			return
		}
		normalizeConfig(&cfg)
		old := currentConfig()
		if err := saveConfigToFile(&cfg, configPath()); err != nil {
			errJSON(w, 500, "保存配置失败: "+err.Error())
			return
		}
		setGlobalConfig(&cfg)
		hub.setMax(cfg.WebConfig.LogBuffer)
		logOkf("[cfg] 配置已保存到 %s", configPath())
		if old.Proxy != cfg.Proxy {
			go initProxy(cfg.Proxy)
		}
		if old.WebConfig.Host != cfg.WebConfig.Host || old.WebConfig.Port != cfg.WebConfig.Port ||
			old.WebConfig.PathPrefix != cfg.WebConfig.PathPrefix || old.WebConfig.Enabled != cfg.WebConfig.Enabled ||
			old.WebConfig.AutoRun != cfg.WebConfig.AutoRun {
			logWarnf("[cfg] Web 服务参数已变更, 需重启程序后生效")
		}
		writeJSON(w, 200, map[string]any{"ok": true, "path": configPath(), "config": currentConfig()})
	default:
		errJSON(w, 405, "方法不允许")
	}
}

func handleConfigReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errJSON(w, 405, "方法不允许")
		return
	}
	cfg := defaultAppConfig()
	normalizeConfig(&cfg)
	if err := saveConfigToFile(&cfg, configPath()); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	setGlobalConfig(&cfg)
	logWarnf("[cfg] 配置已恢复默认(旧配置备份为 config.json.bak)")
	writeJSON(w, 200, map[string]any{"ok": true, "config": currentConfig()})
}

func mergeMaps(dst, src map[string]any) {
	for k, v := range src {
		if vm, ok := v.(map[string]any); ok {
			if dm, ok2 := dst[k].(map[string]any); ok2 {
				mergeMaps(dm, vm)
				continue
			}
		}
		dst[k] = v
	}
}

func handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errJSON(w, 405, "方法不允许")
		return
	}
	var req struct {
		Mode      string         `json:"mode"`
		Save      bool           `json:"save"`
		Overrides map[string]any `json:"overrides"`
	}
	if err := readJSON(r, &req); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	mode := req.Mode
	if mode == "" {
		mode = "full"
	}
	switch mode {
	case "full", "speedtest", "sub", "dns":
	default:
		errJSON(w, 400, "未知任务模式: "+mode)
		return
	}
	cfg := currentConfig()
	if len(req.Overrides) > 0 {
		b, _ := json.Marshal(cfg)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		mergeMaps(m, req.Overrides)
		nb, _ := json.Marshal(m)
		var nc AppConfig
		if err := json.Unmarshal(nb, &nc); err != nil {
			errJSON(w, 400, "临时参数无效: "+err.Error())
			return
		}
		cfg = &nc
	}
	normalizeConfig(cfg)
	if req.Save {
		if err := saveConfigToFile(cfg, configPath()); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		setGlobalConfig(cfg)
	}
	id, err := startJob(mode, cfg)
	if err != nil {
		errJSON(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "job_id": id, "mode": mode})
}

func handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errJSON(w, 405, "方法不允许")
		return
	}
	if !stopJob() {
		errJSON(w, 409, "当前没有正在运行的任务")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		hub.clear()
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 1000
	}
	logs := hub.since(since, limit)
	writeJSON(w, 200, map[string]any{"ok": true, "logs": logs, "count": len(logs)})
}

func handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		errJSON(w, 500, "当前环境不支持 SSE")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)

	ch := hub.subscribe()
	defer hub.unsubscribe(ch)
	send := func(ev Event) bool {
		b, err := json.Marshal(ev.Data)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if !send(Event{Type: "state", Data: buildState()}) {
		return
	}
	for _, e := range hub.since(0, 300) {
		if !send(Event{Type: "log", Data: e}) {
			return
		}
	}
	tk := time.NewTicker(15 * time.Second)
	defer tk.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok || !send(ev) {
				return
			}
		case <-tk.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func handleResults(w http.ResponseWriter, r *http.Request) {
	cfg := currentConfig()
	path := r.URL.Query().Get("file")
	if path == "" {
		path = cfg.OutFile
	}
	abs, ok := resultAllowed(path)
	if !ok {
		errJSON(w, 403, "路径不在允许范围内: "+path)
		return
	}
	if r.URL.Query().Get("download") == "1" {
		serveDownload(w, abs)
		return
	}
	if _, err := os.Stat(abs); err != nil {
		writeJSON(w, 200, map[string]any{"ok": true, "file": abs, "exists": false, "header": csvHeader, "rows": [][]string{}, "total": 0})
		return
	}
	header, rows, hasHeader, err := readCSVTable(abs)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	total := len(rows)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 5000
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	resp := map[string]any{
		"ok": true, "file": abs, "exists": true, "header": header, "rows": rows,
		"total": total, "shown": len(rows), "has_header": hasHeader,
	}
	if st, err := os.Stat(abs); err == nil {
		resp["size"] = st.Size()
		resp["mtime"] = st.ModTime().Format("2006-01-02 15:04:05")
	}
	writeJSON(w, 200, resp)
}

func handleHistory(w http.ResponseWriter, r *http.Request) {
	cfg := currentConfig()
	if cfg.History == "" {
		writeJSON(w, 200, map[string]any{"ok": true, "file": "", "segments": []HistorySegment{}, "total": 0})
		return
	}
	abs, ok := resultAllowed(cfg.History)
	if !ok {
		errJSON(w, 403, "历史文件路径不在允许范围内")
		return
	}
	if r.URL.Query().Get("download") == "1" {
		serveDownload(w, abs)
		return
	}
	if _, err := os.Stat(abs); err != nil {
		writeJSON(w, 200, map[string]any{"ok": true, "file": abs, "exists": false, "segments": []HistorySegment{}, "total": 0})
		return
	}
	segs, total, err := readHistorySegments(abs)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "file": abs, "exists": true, "segments": segs, "total": total, "header": csvHeader})
}

func handleHistoryTrim(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errJSON(w, 405, "方法不允许")
		return
	}
	var req struct {
		Limit int `json:"limit"`
	}
	_ = readJSON(r, &req)
	cfg := currentConfig()
	if cfg.History == "" {
		errJSON(w, 400, "未配置 history 文件")
		return
	}
	if req.Limit <= 0 {
		req.Limit = cfg.HistoryLimit
	}
	if req.Limit <= 0 {
		errJSON(w, 400, "limit 必须大于 0")
		return
	}
	removed, err := trimHistory(cfg.History, req.Limit)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	logOkf("[history] 手动清理完成: 上限 %d 行, 删除 %d 行", req.Limit, removed)
	writeJSON(w, 200, map[string]any{"ok": true, "removed": removed})
}

func handleHistoryClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errJSON(w, 405, "方法不允许")
		return
	}
	cfg := currentConfig()
	if cfg.History == "" {
		errJSON(w, 400, "未配置 history 文件")
		return
	}
	if err := os.WriteFile(cfg.History, []byte(""), 0644); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	logWarnf("[history] 已清空历史归档文件 %s", cfg.History)
	writeJSON(w, 200, map[string]any{"ok": true})
}

type fileEntry struct {
	Path    string `json:"path"`
	Name    string `json:"name"`
	Rel     string `json:"rel"`
	Size    int64  `json:"size"`
	ModTime string `json:"mtime"`
	Ext     string `json:"ext"`
}

func handleFiles(w http.ResponseWriter, r *http.Request) {
	root := subOutputRoot()
	out := []fileEntry{}
	exists := false
	if st, err := os.Stat(root); err == nil && st.IsDir() {
		exists = true
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if p != root && strings.HasPrefix(d.Name(), ".") {
					return fs.SkipDir
				}
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			out = append(out, fileEntry{
				Path: p, Name: d.Name(), Rel: rel, Size: info.Size(),
				ModTime: info.ModTime().Format("2006-01-02 15:04:05"),
				Ext:     strings.ToLower(filepath.Ext(p)),
			})
			if len(out) >= 800 {
				return fs.SkipAll
			}
			return nil
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime > out[j].ModTime })
	writeJSON(w, 200, map[string]any{"ok": true, "files": out, "root": root, "exists": exists})
}

func handleFile(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if p == "" {
		errJSON(w, 400, "缺少 path 参数")
		return
	}
	abs, ok := underRoot(subOutputRoot(), p)
	if !ok {
		errJSON(w, 403, "只允许访问订阅输出目录内的文件")
		return
	}
	if r.URL.Query().Get("download") == "1" {
		serveDownload(w, abs)
		return
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		errJSON(w, 404, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func handleSourcesPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errJSON(w, 405, "方法不允许")
		return
	}
	if isRunning() {
		errJSON(w, 409, "任务运行中, 请先停止后再解析输入源")
		return
	}
	var req struct {
		Sources []string `json:"sources"`
		Depth   int      `json:"depth"`
	}
	_ = readJSON(r, &req)
	st := settingsFromConfig(currentConfig())
	src := dedupeStrings(req.Sources)
	if len(src) == 0 {
		src = st.Sources
	}
	depth := st.SourceDepth
	if req.Depth > 0 {
		depth = req.Depth
	}
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	setPhase("preview", "输入源解析", 0)
	ips, err := fetchIPsFromSources(ctx, st, src, depth)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	shown := ips
	if len(shown) > 1000 {
		shown = shown[:1000]
	}
	setPhase("idle", "空闲", 0)
	writeJSON(w, 200, map[string]any{"ok": true, "count": len(ips), "ips": shown, "sources": src, "depth": depth})
}

func handleProxyTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errJSON(w, 405, "方法不允许")
		return
	}
	var req struct {
		Proxy string `json:"proxy"`
		Apply bool   `json:"apply"`
	}
	_ = readJSON(r, &req)
	raw := strings.TrimSpace(req.Proxy)
	out := map[string]any{"ok": true, "proxy": raw}
	if raw == "" {
		out["active"] = false
		out["message"] = "空代理 = 直连"
		if req.Apply {
			initProxy("")
			out["applied"] = true
		}
		writeJSON(w, 200, out)
		return
	}
	u, err := url.Parse(raw)
	if err != nil {
		out["ok"] = false
		out["message"] = "代理地址解析失败: " + err.Error()
		writeJSON(w, 200, out)
		return
	}
	tr := &http.Transport{Proxy: http.ProxyURL(u)}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	req2, _ := http.NewRequestWithContext(r.Context(), "GET", currentConfig().ProbeURL, nil)
	req2.Header.Set("User-Agent", "Mozilla/5.0")
	start := time.Now()
	resp, err := client.Do(req2)
	if err != nil {
		out["ok"] = false
		out["message"] = "代理连接失败: " + err.Error()
		writeJSON(w, 200, out)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	out["status"] = resp.StatusCode
	out["latency_ms"] = time.Since(start).Milliseconds()
	out["body"] = string(body)
	out["ok"] = resp.StatusCode == 200
	if resp.StatusCode != 200 {
		out["message"] = fmt.Sprintf("probe 返回 %d", resp.StatusCode)
	}
	if req.Apply {
		initProxy(raw)
		out["applied"] = true
	}
	writeJSON(w, 200, out)
}

func handleIPTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errJSON(w, 405, "方法不允许")
		return
	}
	var req struct {
		IP    string `json:"ip"`
		Port  int    `json:"port"`
		Speed bool   `json:"speed"`
	}
	if err := readJSON(r, &req); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	req.IP = strings.TrimSpace(req.IP)
	if net.ParseIP(req.IP) == nil {
		errJSON(w, 400, "无效的 IP 地址")
		return
	}
	if req.Port <= 0 || req.Port > 65535 {
		req.Port = 443
	}
	st := settingsFromConfig(currentConfig())
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	out := map[string]any{"ok": true, "ip": req.IP, "port": req.Port}
	d := &net.Dialer{Timeout: st.TCPTimeout}
	start := time.Now()
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(req.IP, strconv.Itoa(req.Port)))
	if err != nil {
		out["tcp_ok"] = false
		out["error"] = err.Error()
		writeJSON(w, 200, out)
		return
	}
	conn.Close()
	out["tcp_ok"] = true
	out["tcp_ms"] = time.Since(start).Milliseconds()
	if pb, perr := probeIP(ctx, st, req.IP, req.Port); perr != nil {
		out["probe_error"] = perr.Error()
	} else {
		out["probe"] = pb
		out["country"] = countryNameZh(pb.Country)
	}
	if req.Speed {
		sp := getDownloadSpeed(ctx, st, req.IP, req.Port)
		out["speed_mbps"] = sp
		out["speed"] = fmt.Sprintf("%.2f MB/s", sp)
	}
	logf("[tool] 单 IP 测试 %s:%d 完成", req.IP, req.Port)
	writeJSON(w, 200, out)
}

func handleSubPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errJSON(w, 405, "方法不允许")
		return
	}
	cfg := currentConfig()
	files, err := BuildSubscriptions(SubCreateConfig{OutFile: cfg.OutFile, Sub: cfg.SubConfig})
	if err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "files": files})
}

func handleSubGenerate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errJSON(w, 405, "方法不允许")
		return
	}
	cfg := currentConfig()
	files, err := GenerateAllSubscriptions(SubCreateConfig{OutFile: cfg.OutFile, Sub: cfg.SubConfig})
	if err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	var names []string
	for _, f := range files {
		if f.Written {
			names = append(names, f.Path)
		}
	}
	updateStats(func(s *RunStats) { s.SubFiles = names })
	writeJSON(w, 200, map[string]any{"ok": true, "files": files})
}

func handleDNSPlan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errJSON(w, 405, "方法不允许")
		return
	}
	cfg := currentConfig()
	results, err := loadSpeedResultsFromFile(cfg.OutFile)
	if err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	items := PlanDNSRecords(results, cfg.DNSConfig)
	writeJSON(w, 200, map[string]any{"ok": true, "items": items, "count": len(items), "enabled": cfg.DNSConfig.Enabled})
}

func handleDNSRecords(w http.ResponseWriter, r *http.Request) {
	cfg := currentConfig()
	switch r.Method {
	case http.MethodGet:
		recs, err := ListCFRecords(cfg.DNSConfig, r.URL.Query().Get("name"))
		if err != nil {
			errJSON(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "records": recs})
	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if err := DeleteCFRecord(cfg.DNSConfig, id); err != nil {
			errJSON(w, 400, err.Error())
			return
		}
		logWarnf("[dns] 已删除记录 %s", id)
		writeJSON(w, 200, map[string]any{"ok": true})
	default:
		errJSON(w, 405, "方法不允许")
	}
}

const indexHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>IPtest 控制台</title>
<style>
*{box-sizing:border-box}
body{margin:0;background:#0e1320;color:#dde5f2;font:14px/1.6 system-ui,-apple-system,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif}
a{color:#6cb6ff}
header{position:sticky;top:0;z-index:30;display:flex;flex-wrap:wrap;gap:10px;align-items:center;padding:10px 16px;background:#141b2c;border-bottom:1px solid #24304a}
.brand{font-weight:700;font-size:16px}
.spacer{flex:1}
button{background:#22304d;color:#dde5f2;border:1px solid #2e3f63;border-radius:6px;padding:6px 12px;cursor:pointer;font-size:13px}
button:hover{background:#2b3c60}
button.primary{background:#2563eb;border-color:#2563eb}
button.danger{background:#7f1d1d;border-color:#991b1b}
button.mini{padding:3px 8px;font-size:12px}
button:disabled{opacity:.45;cursor:not-allowed}
.pill{padding:3px 10px;border-radius:999px;background:#24304a;font-size:12px}
.pill.run{background:#1d4ed8}.pill.ok{background:#15803d}.pill.err{background:#991b1b}.pill.warn{background:#a16207}
nav{display:flex;flex-wrap:wrap;gap:6px;padding:8px 16px;background:#111827;border-bottom:1px solid #24304a}
nav button{background:transparent;border-color:transparent}
nav button.active{background:#22304d;border-color:#2e3f63}
main{padding:16px;max-width:1500px;margin:0 auto}
section{display:none}section.active{display:block}
.card{background:#141b2c;border:1px solid #24304a;border-radius:10px;padding:14px;margin-bottom:14px}
.card h3{margin:0 0 10px;font-size:14px;color:#9fb0cc}
.card h4{margin:12px 0 8px;font-size:13px;color:#9fb0cc}
.grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(260px,1fr));gap:12px}
.fld label{display:block;font-size:12px;color:#9fb0cc;margin-bottom:4px}
.fld.wide{grid-column:1/-1}
input,textarea,select{width:100%;background:#0e1320;color:#dde5f2;border:1px solid #2e3f63;border-radius:6px;padding:6px 8px;font-size:13px;font-family:inherit}
textarea{font-family:ui-monospace,Consolas,monospace;resize:vertical}
input[type=checkbox]{width:auto}
.chk{display:inline-flex;align-items:center;gap:6px;font-size:13px;color:#dde5f2;margin-right:12px}
.help{font-size:11px;color:#6f7f9c;margin-top:3px;word-break:break-all}
.cards{display:grid;grid-template-columns:repeat(auto-fill,minmax(140px,1fr));gap:10px}
.stat{background:#0e1320;border:1px solid #24304a;border-radius:8px;padding:10px}
.stat b{display:block;font-size:20px;color:#fff}
.stat span{font-size:12px;color:#8b9ab8}
.bar{height:10px;background:#0e1320;border:1px solid #24304a;border-radius:999px;overflow:hidden}
.bar i{display:block;height:100%;background:linear-gradient(90deg,#2563eb,#22d3ee);width:0}
.chips{display:flex;flex-wrap:wrap;gap:6px;margin-top:8px}
.chip{background:#0e1320;border:1px solid #24304a;border-radius:6px;padding:3px 8px;font-size:12px;color:#9fb0cc}
table{width:100%;border-collapse:collapse;font-size:12.5px}
th,td{border-bottom:1px solid #22304d;padding:5px 8px;text-align:left;white-space:nowrap}
th{color:#9fb0cc;position:sticky;top:0;background:#141b2c}
th.s{cursor:pointer}
tbody tr:hover{background:#1a2338}
.wrap{max-height:540px;overflow:auto;border:1px solid #24304a;border-radius:8px}
.logs{background:#0b1020;border:1px solid #24304a;border-radius:8px;padding:8px;height:56vh;overflow:auto;font:12px/1.5 ui-monospace,Consolas,monospace;white-space:pre-wrap;word-break:break-all}
.logs.tall{height:66vh}
.log{color:#b9c6dd}.log.ok{color:#4ade80}.log.warn{color:#fbbf24}.log.error{color:#f87171}.log.debug{color:#6f7f9c}
.row{display:flex;flex-wrap:wrap;gap:8px;align-items:center;margin-bottom:10px}
.muted{color:#7b8aa6;font-size:12px}
.good{color:#4ade80}.mid{color:#fbbf24}
pre{background:#0b1020;border:1px solid #24304a;border-radius:8px;padding:10px;max-height:420px;overflow:auto;font-size:12px;white-space:pre-wrap;word-break:break-all}
#toast{position:fixed;right:16px;bottom:16px;z-index:60;display:flex;flex-direction:column;gap:8px}
.titem{background:#22304d;border:1px solid #2e3f63;border-radius:8px;padding:8px 12px;max-width:380px}
.titem.ok{background:#15803d}.titem.err{background:#991b1b}
.tplrow{border:1px dashed #2e3f63;border-radius:8px;padding:10px;margin-bottom:10px}
details{border:1px solid #24304a;border-radius:8px;padding:8px;margin-bottom:8px;background:#0e1320}
summary{cursor:pointer;color:#9fb0cc}
</style>
</head>
<body>
<header>
  <div class="brand">IPtest 控制台</div>
  <span id="badge" class="pill">加载中</span>
  <span id="conn" class="pill">SSE</span>
  <span id="dirty" class="pill warn" style="display:none">配置未保存</span>
  <div class="spacer"></div>
  <label class="chk"><input type="checkbox" id="autorun_save" checked> 运行前保存配置</label>
  <button onclick="saveConfig()">保存配置</button>
  <button class="primary" id="btnfull" onclick="runJob('full')">完整流程</button>
  <button id="btnspeed" onclick="runJob('speedtest')">仅测速</button>
  <button class="danger" id="btnstop" onclick="stopJob()">停止</button>
</header>
<nav>
  <button data-tab="dash" class="active">控制台</button>
  <button data-tab="cfg">配置</button>
  <button data-tab="res">结果</button>
  <button data-tab="his">历史归档</button>
  <button data-tab="sub">订阅</button>
  <button data-tab="dns">DNS</button>
  <button data-tab="files">文件</button>
  <button data-tab="tools">工具</button>
  <button data-tab="logs">日志</button>
</nav>
<main>
  <section id="tab-dash" class="active">
    <div class="card">
      <h3>运行状态</h3>
      <div class="row"><span id="phasetext" class="muted">-</span><div class="spacer"></div><span id="elapsed" class="muted"></span></div>
      <div class="bar"><i id="progbar"></i></div>
      <div class="chips" id="chips"></div>
      <div class="row" style="margin-top:12px">
        <button onclick="runJob('sub')">仅生成订阅</button>
        <button onclick="runJob('dns')">仅推送 DNS</button>
        <button onclick="loadState()">刷新状态</button>
        <button onclick="showTab('res');loadResults()">查看结果</button>
      </div>
      <div id="joberr" class="help"></div>
    </div>
    <div class="card"><h3>统计</h3><div class="cards" id="cards"></div></div>
    <div class="card">
      <h3>实时日志</h3>
      <div class="row"><label class="chk"><input type="checkbox" id="logauto" checked> 自动滚动</label><button class="mini" onclick="switchTab('logs')">完整日志</button><button class="mini" onclick="clearLogs()">清空</button></div>
      <div class="logs" id="logtail"></div>
    </div>
  </section>

  <section id="tab-cfg">
    <div class="card">
      <div class="row">
        <button class="primary" onclick="saveConfig()">保存配置</button>
        <button onclick="reloadConfig()">重新载入</button>
        <button class="danger" onclick="resetConfig()">恢复默认</button>
        <span id="cfgdirty" class="pill warn" style="display:none">有未保存变更</span>
        <span id="cfgpath" class="muted"></span>
      </div>
      <div id="restartHint" class="help"></div>
      <div id="cfgform"></div>
    </div>
  </section>

  <section id="tab-res">
    <div class="card">
      <div class="row">
        <button onclick="loadResults()">刷新</button>
        <input type="text" id="resq" placeholder="搜索 IP / 国家 / 数据中心 / ASN" style="max-width:320px" oninput="renderResults()">
        <select id="reslimit" onchange="renderResults()" style="max-width:140px"><option>100</option><option selected>300</option><option>1000</option><option>5000</option></select>
        <a id="resdl" href="#" target="_blank">下载 CSV</a>
      </div>
      <div id="resinfo" class="muted"></div>
      <div class="wrap" style="margin-top:10px"><div id="restable"></div></div>
    </div>
  </section>

  <section id="tab-his">
    <div class="card">
      <div class="row">
        <button onclick="loadHistory()">刷新</button>
        <input type="number" id="hislimit" style="max-width:140px" placeholder="行数上限">
        <button onclick="trimHistory()">按上限清理</button>
        <button class="danger" onclick="clearHistory()">清空归档</button>
        <a id="hisdl" href="#" target="_blank">下载归档</a>
      </div>
      <div id="hisinfo" class="muted"></div>
      <div id="hisbox" style="margin-top:10px"></div>
    </div>
  </section>

  <section id="tab-sub">
    <div class="card">
      <div class="row">
        <button onclick="previewSub()">预览节点</button>
        <button class="primary" onclick="genSub()">生成订阅文件</button>
        <span class="muted">参数在 配置 → 订阅 sub_config 中修改</span>
      </div>
      <div id="subbox"></div>
    </div>
  </section>

  <section id="tab-dns">
    <div class="card">
      <div class="row">
        <button onclick="dnsPlan()">预览推送计划</button>
        <button class="primary" onclick="runJob('dns')">执行推送</button>
        <button onclick="dnsRecords()">拉取现有记录</button>
        <input type="text" id="dnsname" placeholder="按域名过滤(可选)" style="max-width:240px">
      </div>
      <h4>推送计划</h4><div class="wrap"><div id="dnsplanbox"></div></div>
      <h4>Cloudflare 现有记录</h4><div class="wrap"><div id="dnsrecbox"></div></div>
    </div>
  </section>

  <section id="tab-files">
    <div class="card">
      <div class="row"><button onclick="loadFiles()">刷新</button><span id="fileinfo" class="muted"></span></div>
      <div class="wrap"><div id="filesbox"></div></div>
      <h4>文件内容</h4>
      <div class="row"><button class="mini" onclick="decodeView()">Base64 解码</button><span id="viewname" class="muted"></span></div>
      <pre id="fileview"></pre>
    </div>
  </section>

  <section id="tab-tools">
    <div class="card">
      <h3>输入源解析预览(不测速)</h3>
      <textarea id="srcinput" rows="5" spellcheck="false" placeholder="每行一个: 本地路径 / http(s) URL / file://"></textarea>
      <div class="row" style="margin-top:8px">
        <input type="number" id="srcdepth" style="max-width:120px" placeholder="深度">
        <button onclick="previewSources()">开始解析</button>
      </div>
      <pre id="srcout"></pre>
    </div>
    <div class="card">
      <h3>代理测试</h3>
      <div class="row">
        <input type="text" id="proxyinput" placeholder="socks5://127.0.0.1:10808 或 http://127.0.0.1:7890" style="max-width:420px">
        <label class="chk"><input type="checkbox" id="proxyapply" checked> 测试成功后应用</label>
        <button onclick="testProxy()">测试</button>
      </div>
      <pre id="proxyout"></pre>
    </div>
    <div class="card">
      <h3>单 IP 探测 / 测速</h3>
      <div class="row">
        <input type="text" id="tip" placeholder="IP 地址" style="max-width:220px">
        <input type="number" id="tport" value="443" style="max-width:110px">
        <label class="chk"><input type="checkbox" id="tspeed" checked> 同时下载测速</label>
        <button onclick="testIP()">测试</button>
      </div>
      <pre id="ipout"></pre>
    </div>
  </section>

  <section id="tab-logs">
    <div class="card">
      <div class="row">
        <input type="text" id="logq" placeholder="过滤关键字" style="max-width:280px" oninput="renderLogs()">
        <select id="loglvl" style="max-width:140px" onchange="renderLogs()">
          <option value="">全部级别</option><option value="info">info</option><option value="ok">ok</option><option value="warn">warn</option><option value="error">error</option><option value="debug">debug</option>
        </select>
        <label class="chk"><input type="checkbox" id="logdebug" onchange="renderLogs()"> 显示 debug</label>
        <button onclick="clearLogs()">清空</button>
        <button onclick="exportLogs()">导出</button>
        <span id="logcount" class="muted"></span>
      </div>
      <div class="logs tall" id="logfull"></div>
    </div>
  </section>
</main>
<div id="toast"></div>
<script>
var BASE='__BASE__';
var CFG=null,ST=null,LOGS=[],FILES=[],DNSREC=[],VIEWTEXT='',BASELINE='',CURTAB='dash';
var R={file:'',header:[],rows:[],sort:-1,dir:1,meta:null};
var SCHEMA=[
 {title:'输入与输出',items:[
  {k:'file',t:'list',l:'输入源 file',h:'每行一个: 本地路径 / http(s) URL / file://'},
  {k:'outfile',t:'text',l:'结果文件 outfile'},
  {k:'proxy',t:'text',l:'代理 proxy',h:'socks5://127.0.0.1:10808; 空=直连; 仅用于远程下载与 DNS 推送'}
 ]},
 {title:'测速参数',items:[
  {k:'max',t:'num',l:'最大并发 max'},
  {k:'speedtest',t:'num',l:'测速协程 speedtest',h:'0 = 禁用下载测速, 只测延迟'},
  {k:'url',t:'text',l:'测速地址 url'},
  {k:'delay',t:'num',l:'延迟阈值 ms delay',h:'0 = 不过滤'},
  {k:'min_speed',t:'num',s:'0.1',l:'合格速度 MB/s min_speed'},
  {k:'min_speed_count',t:'num',l:'合格数量 min_speed_count',h:'与 min_speed 同时>0 时提前停止'},
  {k:'rounds',t:'num',l:'延迟轮数 rounds',h:'仅 speedtest=0 生效'},
  {k:'source_depth',t:'num',l:'来源递归深度 source_depth'}
 ]},
 {title:'超时与探测',items:[
  {k:'tcp_timeout_ms',t:'num',l:'TCP 超时 ms'},
  {k:'probe_timeout_ms',t:'num',l:'真连接超时 ms'},
  {k:'download_timeout_ms',t:'num',l:'下载超时 ms'},
  {k:'source_timeout_ms',t:'num',l:'来源下载超时 ms'},
  {k:'probe_url',t:'text',l:'探测地址 probe_url',h:'SNI 自动从该地址的域名提取, 无需单独配置'}
 ]},
 {title:'历史归档',items:[
  {k:'history_enable',t:'bool',l:'启用归档 history_enable'},
  {k:'history',t:'text',l:'归档文件 history'},
  {k:'history_count',t:'num',l:'每次归档数量 history_count'},
  {k:'history_limit',t:'num',l:'归档行数上限 history_limit',h:'0 = 不限制'}
 ]},
 {title:'订阅 sub_config',tpl:true,items:[
  {k:'sub_config.enabled',t:'bool',l:'启用订阅'},
  {k:'sub_config.output_dir',t:'text',l:'输出目录 output_dir',h:'文件页仅允许浏览/下载此目录'},
  {k:'sub_config.count',t:'num',l:'IP 节点数量 count',h:'0 = 全部; 域名节点按优选域名列表'},
  {k:'sub_config.node_name_tpl',t:'text',l:'节点名模板 node_name_tpl',h:'{index} {country} {countrycode} {ip} {port} {domain}'},
  {k:'sub_config.b64encode',t:'bool',l:'Base64 编码 b64encode'},
  {k:'sub_config.subtype',t:'multi',l:'订阅类型 subtype',opts:['ip','domain']},
  {k:'sub_config.preferred_domains',t:'list',l:'优选域名 preferred_domains'}
 ]},
 {title:'DNS dns_config',items:[
  {k:'dns_config.enabled',t:'bool',l:'启用推送'},
  {k:'dns_config.token',t:'pass',l:'API Token'},
  {k:'dns_config.zone_id',t:'text',l:'Zone ID'},
  {k:'dns_config.domain_tpl',t:'text',l:'域名模板 domain_tpl',h:'{index} {ip} {country} {countrycode} {port}'},
  {k:'dns_config.count',t:'num',l:'推送数量 count',h:'0 = 全部'},
  {k:'dns_config.ttl',t:'num',l:'TTL'},
  {k:'dns_config.proxied',t:'bool',l:'Proxied 橙云'}
 ]},
 {title:'Web 服务 web_config',items:[
  {k:'web_config.enabled',t:'bool',l:'启用 Web 服务',h:'变更需重启服务生效; 关闭后下次启动走 CLI'},
  {k:'web_config.host',t:'text',l:'监听地址 host',h:'变更需重启服务生效'},
  {k:'web_config.port',t:'num',l:'端口 port',h:'变更需重启服务生效'},
  {k:'web_config.path_prefix',t:'text',l:'URL 路径前缀 path_prefix',h:'变更需重启服务生效; 留空=根路径; 例 /iptest, 之后页面与接口全部位于 http://host:port/iptest/ , 便于 nginx 反代与路由隐藏'},
  {k:'web_config.auto_run',t:'bool',l:'启动即自动跑完整流程',h:'变更需重启服务生效'},
  {k:'web_config.log_buffer',t:'num',l:'日志缓冲条数',h:'保存后立即生效'},
  {k:'web_config.username',t:'text',l:'Basic Auth 用户名',h:'保存后立即生效'},
  {k:'web_config.password',t:'pass',l:'Basic Auth 密码',h:'留空=不鉴权; 保存后立即生效, 浏览器需重新登录'}
 ]}
];
function $(id){return document.getElementById(id);}
function U(p){return BASE+p;}
function esc(s){return String(s==null?'':s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');}
function api(p,m,b){
  var o={method:m||'GET',headers:{'Content-Type':'application/json'}};
  if(b!==undefined&&b!==null)o.body=JSON.stringify(b);
  return fetch(p,o).then(function(res){return res.text().then(function(t){
    var d=null;try{d=t?JSON.parse(t):null;}catch(e){d={raw:t};}
    if(!res.ok){throw new Error((d&&(d.error||d.message))||('HTTP '+res.status));}
    return d;});});
}
function toast(m,k){var d=document.createElement('div');d.className='titem '+(k||'');d.textContent=m;$('toast').appendChild(d);setTimeout(function(){d.remove();},4200);}
function getP(o,p){var a=p.split('.');for(var i=0;i<a.length;i++){if(o==null)return undefined;o=o[a[i]];}return o;}
function setP(o,p,v){var a=p.split('.');for(var i=0;i<a.length-1;i++){if(typeof o[a[i]]!=='object'||o[a[i]]===null)o[a[i]]={};o=o[a[i]];}o[a[a.length-1]]=v;}
function fid(k){return 'f_'+k.replace(/\./g,'_');}
/* ---------- 页签 ---------- */
function showTab(n){
  CURTAB=n;
  var bs=document.querySelectorAll('nav button');
  for(var i=0;i<bs.length;i++)bs[i].classList.toggle('active',bs[i].getAttribute('data-tab')===n);
  var ss=document.querySelectorAll('main section');
  for(var j=0;j<ss.length;j++)ss[j].classList.toggle('active',ss[j].id==='tab-'+n);
  if(n==='res'&&!R.rows.length)loadResults();
  if(n==='his')loadHistory();
  if(n==='files')loadFiles();
  if(n==='logs')renderLogs();
  if(n==='cfg')refreshDirty();
}
function switchTab(n){
  if(CURTAB==='cfg'&&n!=='cfg'&&isDirty()){
    if(confirm('检测到配置有未保存的变更。\n\n【确定】保存配置后切换\n【取消】暂不保存直接切换(变更仍保留在表单中)')){
      saveConfig().then(function(){showTab(n);});
      return;
    }
  }
  showTab(n);
}
/* ---------- 配置表单 ---------- */
function buildConfigForm(){
  var h='';
  SCHEMA.forEach(function(g){
    h+='<div class="card"><h3>'+g.title+'</h3><div class="grid">';
    g.items.forEach(function(f){
      var id=fid(f.k);
      h+='<div class="fld'+(f.t==='list'?' wide':'')+'"><label>'+f.l+'</label>';
      if(f.t==='bool')h+='<label class="chk"><input type="checkbox" id="'+id+'"> 启用</label>';
      else if(f.t==='list')h+='<textarea id="'+id+'" rows="4" spellcheck="false"></textarea>';
      else if(f.t==='multi'){h+='<div>';f.opts.forEach(function(o){h+='<label class="chk"><input type="checkbox" id="'+id+'_'+o+'"> '+o+'</label>';});h+='</div>';}
      else if(f.t==='pass')h+='<input type="password" id="'+id+'" autocomplete="new-password">';
      else if(f.t==='num')h+='<input type="number" id="'+id+'" step="'+(f.s||'1')+'">';
      else h+='<input type="text" id="'+id+'" spellcheck="false">';
      if(f.h)h+='<div class="help">'+f.h+'</div>';
      h+='</div>';
    });
    h+='</div>';
    if(g.tpl)h+='<div id="tplbox"></div>';
    h+='</div>';
  });
  $('cfgform').innerHTML=h;
}
function fillConfigForm(){
  if(!CFG)return;
  SCHEMA.forEach(function(g){g.items.forEach(function(f){
    var v=getP(CFG,f.k),id=fid(f.k);
    if(f.t==='multi'){f.opts.forEach(function(o){var e=$(id+'_'+o);if(e)e.checked=(v||[]).indexOf(o)>=0;});return;}
    var el=$(id);if(!el)return;
    if(f.t==='bool')el.checked=!!v;
    else if(f.t==='list')el.value=(v||[]).join('\n');
    else if(f.t==='num')el.value=(v==null?0:v);
    else el.value=(v==null?'':v);
  });});
  renderTemplates();
  if($('srcinput'))$('srcinput').value=(CFG.file||[]).join('\n');
  if($('srcdepth'))$('srcdepth').value=CFG.source_depth;
  if($('proxyinput'))$('proxyinput').value=CFG.proxy||'';
  if($('hislimit'))$('hislimit').value=CFG.history_limit;
}
function applyCfg(cfg){CFG=cfg;buildConfigForm();fillConfigForm();markClean();}
function collectConfig(){
  var c=JSON.parse(JSON.stringify(CFG||{}));
  SCHEMA.forEach(function(g){g.items.forEach(function(f){
    var id=fid(f.k),v;
    if(f.t==='multi'){v=[];f.opts.forEach(function(o){var e=$(id+'_'+o);if(e&&e.checked)v.push(o);});}
    else{
      var el=$(id);
      if(!el){v=getP(CFG,f.k);}
      else if(f.t==='bool')v=el.checked;
      else if(f.t==='list')v=el.value.split('\n').map(function(s){return s.trim();}).filter(function(s){return s;});
      else if(f.t==='num')v=parseFloat(el.value)||0;
      else v=el.value;
    }
    setP(c,f.k,v);
  });});
  if(!c.sub_config)c.sub_config={};
  c.sub_config.templates=collectTemplates();
  return c;
}
function renderTemplates(){
  var box=$('tplbox');if(!box||!CFG)return;
  var ts=(CFG.sub_config&&CFG.sub_config.templates)||[];
  var h='<h4>节点模板 templates <button class="mini" onclick="addTpl()">+ 添加模板</button></h4>';
  h+='<div class="help">节点模板变量: {ip} {domain} {port} {node_name_tpl} ; 节点名模板变量: {index} {country} {countrycode} {ip} {port} {domain}</div>';
  ts.forEach(function(t,i){
    h+='<div class="tplrow"><div class="grid">'
      +'<div class="fld"><label>IP 订阅文件名</label><input type="text" id="tpl_ip_'+i+'" value="'+esc(t.ip_file_name)+'"></div>'
      +'<div class="fld"><label>域名订阅文件名</label><input type="text" id="tpl_dom_'+i+'" value="'+esc(t.domain_file_name)+'"></div>'
      +'</div><div class="fld wide" style="margin-top:8px"><label>模板内容</label><textarea id="tpl_t_'+i+'" rows="3" spellcheck="false">'+esc(t.template)+'</textarea></div>'
      +'<div class="row" style="margin-top:6px"><button class="mini danger" onclick="delTpl('+i+')">删除此模板</button></div></div>';
  });
  box.innerHTML=h;
}
function collectTemplates(){
  var ts=(CFG&&CFG.sub_config&&CFG.sub_config.templates)||[],out=[];
  for(var i=0;i<ts.length;i++){
    var a=$('tpl_ip_'+i);if(!a)continue;
    out.push({ip_file_name:a.value.trim(),domain_file_name:$('tpl_dom_'+i).value.trim(),template:$('tpl_t_'+i).value});
  }
  return out;
}
function addTpl(){CFG.sub_config.templates=collectTemplates();CFG.sub_config.templates.push({ip_file_name:'subip.txt',domain_file_name:'subdomain.txt',template:''});renderTemplates();refreshDirty();}
function delTpl(i){var t=collectTemplates();t.splice(i,1);CFG.sub_config.templates=t;renderTemplates();refreshDirty();}
/* ---------- 未保存检测 ---------- */
function cfgSignature(){try{return JSON.stringify(collectConfig());}catch(e){return '';}}
function isDirty(){if(!CFG||!BASELINE)return false;return cfgSignature()!==BASELINE;}
function markClean(){BASELINE=cfgSignature();refreshDirty();}
function refreshDirty(){
  var d=isDirty();
  if($('dirty'))$('dirty').style.display=d?'':'none';
  if($('cfgdirty'))$('cfgdirty').style.display=d?'':'none';
}
/* ---------- 配置读写 ---------- */
function loadConfig(reload){
  return api(U('/api/config')+(reload?'?reload=1':'')).then(function(d){
    if($('cfgpath'))$('cfgpath').textContent=d.path;
    applyCfg(d.config);
    return CFG;
  }).catch(function(e){toast('加载配置失败: '+e.message,'err');});
}
function reloadConfig(){
  if(isDirty()&&!confirm('重新载入会丢弃当前未保存的变更, 继续?'))return;
  loadConfig(true).then(function(){toast('已从配置文件重新载入','ok');});
}
function saveConfig(silent){
  if(!CFG)return Promise.reject(new Error('配置尚未加载'));
  return api(U('/api/config'),'POST',collectConfig()).then(function(d){
    applyCfg(d.config);
    if(!silent)toast('配置已保存','ok');
    loadState();
    return CFG;
  }).catch(function(e){toast('保存失败: '+e.message,'err');throw e;});
}
function resetConfig(){
  if(!confirm('恢复默认配置? 当前配置会备份为 config.json.bak'))return;
  api(U('/api/config/reset'),'POST',{}).then(function(d){applyCfg(d.config);toast('已恢复默认','ok');loadState();}).catch(function(e){toast(e.message,'err');});
}
/* ---------- 状态 ---------- */
function loadState(){return api(U('/api/state')).then(function(d){ST=d;renderState();}).catch(function(e){});}
function renderState(){
  if(!ST)return;
  var j=ST.job,b=$('badge');
  if(j.running){b.className='pill run';b.textContent='运行中: '+(j.phase_label||j.phase);}
  else if(j.phase==='error'){b.className='pill err';b.textContent='失败';}
  else if(j.phase==='stopped'){b.className='pill warn';b.textContent='已停止';}
  else if(j.phase==='done'){b.className='pill ok';b.textContent='完成';}
  else{b.className='pill';b.textContent='空闲';}
  $('btnstop').disabled=!j.running;
  $('btnfull').disabled=j.running;$('btnspeed').disabled=j.running;
  $('elapsed').textContent=j.started_at?('开始 '+j.started_at+' / 耗时 '+j.elapsed_sec+'s'):'';
  $('joberr').textContent=j.error?('错误: '+j.error):'';
  renderProgress(j.progress);
  var s=j.stats,st=j.settings||{};
  var cards=[['输入源',s.sources],['提取 IP',s.total_ips],['有效 IP',s.valid],['已测速',s.speed_tested],['合格 IP',s.qualified],['结果行数',s.result_rows],['最高速度',(s.best_speed||0).toFixed(2)+' MB/s'],['最低延迟',(s.best_latency||0)+' ms'],['订阅文件',(s.sub_files||[]).length],['DNS 成功/失败',(s.dns_pushed||0)+'/'+(s.dns_failed||0)]];
  $('cards').innerHTML=cards.map(function(c){return '<div class="stat"><b>'+esc(c[1])+'</b><span>'+esc(c[0])+'</span></div>';}).join('');
  var wa=ST.web_active||{},ws=ST.web||{};
  var chips=['并发 '+st.max,'测速协程 '+st.speedtest,'延迟阈值 '+st.delay+'ms','轮数 '+st.rounds,'合格 '+(st.min_speed||0)+'MB/s x'+st.min_speed_count,'TCP '+Math.round((st.tcp_timeout||0)/1e6)+'ms','下载 '+Math.round((st.download_timeout||0)/1e6)+'ms','代理 '+(ST.proxy.active?ST.proxy.configured:'直连'),'订阅 '+(ST.summary.sub_enabled?'开':'关'),'DNS '+(ST.summary.dns_enabled?'开':'关'),'归档 '+(ST.summary.history_enable?'开':'关'),'访问前缀 '+(wa.path_prefix||'/'),'结果 '+ST.summary.outfile];
  $('chips').innerHTML=chips.map(function(c){return '<span class="chip">'+esc(c)+'</span>';}).join('');
  var need=(wa.host!==ws.host)||(wa.port!==ws.port)||((wa.path_prefix||'')!==(ws.path_prefix||''))||(wa.host===undefined);
  if($('restartHint'))$('restartHint').textContent=need?('注意: Web 服务参数已修改, 需重启程序后生效。当前生效: http://'+wa.host+':'+wa.port+(wa.path_prefix||'')+'/'):'';
}
function renderProgress(p){
  if(!p)return;
  var pct=p.percent||0;
  $('progbar').style.width=Math.max(0,Math.min(100,pct))+'%';
  $('phasetext').textContent=(p.label||p.phase||'-')+(p.total?(' '+p.done+'/'+p.total+' ('+pct.toFixed(1)+'%)'):'');
}
function runJob(mode){
  var auto=$('autorun_save').checked;
  if(!auto&&isDirty()&&!confirm('配置有未保存的变更, 本次运行将使用【已保存】的配置。\n\n继续运行?'))return;
  var pre=auto?saveConfig(true):Promise.resolve();
  pre.then(function(){showTab('dash');return api(U('/api/run'),'POST',{mode:mode});})
     .then(function(){toast('任务已启动: '+mode,'ok');loadState();})
     .catch(function(e){toast(e.message,'err');});
}
function stopJob(){showTab('dash');api(U('/api/stop'),'POST',{}).then(function(){toast('已发送停止请求','ok');}).catch(function(e){toast(e.message,'err');});}
/* ---------- 日志 ---------- */
function matchLog(e){
  var dbg=$('logdebug')&&$('logdebug').checked;
  if(!dbg&&e.level==='debug')return false;
  var lv=$('loglvl')?$('loglvl').value:'';
  if(lv&&e.level!==lv)return false;
  var q=$('logq')?$('logq').value.toLowerCase():'';
  if(q&&e.msg.toLowerCase().indexOf(q)<0)return false;
  return true;
}
function logNode(e){var d=document.createElement('div');d.className='log '+e.level;d.textContent='['+e.time+'] '+e.msg;return d;}
function pushLog(e){
  LOGS.push(e);if(LOGS.length>8000)LOGS.shift();
  if(!matchLog(e))return;
  var full=$('logfull'),tail=$('logtail');
  full.appendChild(logNode(e));tail.appendChild(logNode(e));
  while(tail.children.length>400)tail.removeChild(tail.firstChild);
  while(full.children.length>8000)full.removeChild(full.firstChild);
  if(!$('logauto')||$('logauto').checked){full.scrollTop=full.scrollHeight;tail.scrollTop=tail.scrollHeight;}
  $('logcount').textContent=LOGS.length+' 条';
}
function renderLogs(){
  var full=$('logfull');full.innerHTML='';var n=0;
  for(var i=0;i<LOGS.length;i++){if(matchLog(LOGS[i])){full.appendChild(logNode(LOGS[i]));n++;}}
  $('logcount').textContent=n+' / '+LOGS.length+' 条';
  full.scrollTop=full.scrollHeight;
}
function clearLogs(){api(U('/api/logs'),'DELETE').then(function(){LOGS=[];$('logfull').innerHTML='';$('logtail').innerHTML='';}).catch(function(e){toast(e.message,'err');});}
function exportLogs(){
  var txt=LOGS.map(function(e){return '['+e.time+']['+e.level+'] '+e.msg;}).join('\n');
  var a=document.createElement('a');a.href=URL.createObjectURL(new Blob([txt],{type:'text/plain'}));a.download='iptest-logs.txt';a.click();
}
function connectSSE(){
  var es=new EventSource(U('/api/events'));
  es.onopen=function(){$('conn').className='pill ok';$('conn').textContent='实时';};
  es.onerror=function(){$('conn').className='pill err';$('conn').textContent='重连中';};
  es.addEventListener('log',function(ev){pushLog(JSON.parse(ev.data));});
  es.addEventListener('state',function(ev){ST=JSON.parse(ev.data);renderState();});
  es.addEventListener('progress',function(ev){renderProgress(JSON.parse(ev.data));});
  es.addEventListener('logs_cleared',function(){LOGS=[];$('logfull').innerHTML='';$('logtail').innerHTML='';});
}
/* ---------- 结果 ---------- */
function numOf(v){var n=parseFloat(String(v).replace(/[^0-9.\-]/g,''));return isNaN(n)?-1:n;}
function loadResults(file){
  return api(U('/api/results')+(file?('?file='+encodeURIComponent(file)):'')).then(function(d){
    R.file=d.file;R.header=d.header||[];R.rows=d.rows||[];R.meta=d;R.sort=-1;
    $('resdl').href=U('/api/results?download=1&file=')+encodeURIComponent(d.file);
    renderResults();
  }).catch(function(e){$('resinfo').textContent='加载失败: '+e.message;R.rows=[];renderResults();});
}
function sortRes(i){if(R.sort===i)R.dir=-R.dir;else{R.sort=i;R.dir=1;}renderResults();}
function renderResults(){
  var q=$('resq').value.toLowerCase(),limit=parseInt($('reslimit').value,10)||300;
  var rows=R.rows.filter(function(r){return !q||r.join(' ').toLowerCase().indexOf(q)>=0;});
  if(R.sort>=0){
    var idx=R.sort,numeric=/延迟|速度|端口|ms|speed/i.test(R.header[idx]||'');
    rows=rows.slice().sort(function(a,b){
      var x=a[idx]||'',y=b[idx]||'';
      if(numeric)return (numOf(x)-numOf(y))*R.dir;
      return String(x).localeCompare(String(y))*R.dir;
    });
  }
  var shown=rows.slice(0,limit),best=0,lat=0;
  R.rows.forEach(function(r){var s=numOf(r[3]);if(s>best)best=s;var l=numOf(r[2]);if(l>0&&(lat===0||l<lat))lat=l;});
  $('resinfo').textContent=(R.meta&&R.meta.exists===false?'文件不存在: ':'')+R.file+' | 总 '+R.rows.length+' 行, 过滤后 '+rows.length+' 行, 显示 '+shown.length+' 行 | 最高 '+(best>0?best.toFixed(2)+' MB/s':'-')+' | 最低延迟 '+(lat||'-')+' ms'+(R.meta&&R.meta.mtime?(' | 更新 '+R.meta.mtime):'');
  var h='<table><thead><tr><th>#</th>';
  for(var i=0;i<R.header.length;i++)h+='<th class="s" onclick="sortRes('+i+')">'+esc(R.header[i])+(R.sort===i?(R.dir>0?' ▲':' ▼'):'')+'</th>';
  h+='</tr></thead><tbody>';
  shown.forEach(function(r,n){
    h+='<tr><td>'+(n+1)+'</td>';
    for(var c=0;c<R.header.length;c++){
      var v=r[c]||'',cls='';
      if(c===3){var s=numOf(v);cls=s>=5?'good':(s>=2?'mid':'');}
      h+='<td class="'+cls+'">'+esc(v)+'</td>';
    }
    h+='</tr>';
  });
  $('restable').innerHTML=h+'</tbody></table>';
}
/* ---------- 历史 ---------- */
function simpleTable(header,rows){
  var h='<table><thead><tr><th>#</th>';
  header.forEach(function(x){h+='<th>'+esc(x)+'</th>';});
  h+='</tr></thead><tbody>';
  rows.forEach(function(r,i){h+='<tr><td>'+(i+1)+'</td>';header.forEach(function(_,c){h+='<td>'+esc(r[c]||'')+'</td>';});h+='</tr>';});
  return h+'</tbody></table>';
}
function loadHistory(){
  return api(U('/api/history')).then(function(d){
    $('hisinfo').textContent=(d.file||'未配置')+' | 共 '+d.total+' 行, '+((d.segments||[]).length)+' 个归档批次';
    $('hisdl').href=U('/api/history?download=1');
    var segs=(d.segments||[]).slice().reverse(),h='';
    segs.forEach(function(s,i){
      h+='<details'+(i===0?' open':'')+'><summary>'+esc(s.time)+' ('+s.rows.length+' 条)</summary><div class="wrap" style="margin-top:8px">'+simpleTable(d.header||[],s.rows)+'</div></details>';
    });
    $('hisbox').innerHTML=h||'<div class="muted">暂无归档数据</div>';
  }).catch(function(e){$('hisinfo').textContent='加载失败: '+e.message;});
}
function trimHistory(){
  api(U('/api/history/trim'),'POST',{limit:parseInt($('hislimit').value,10)||0}).then(function(d){toast('已清理 '+d.removed+' 行','ok');loadHistory();}).catch(function(e){toast(e.message,'err');});
}
function clearHistory(){if(!confirm('确认清空历史归档文件?'))return;api(U('/api/history/clear'),'POST',{}).then(function(){toast('已清空','ok');loadHistory();}).catch(function(e){toast(e.message,'err');});}
/* ---------- 订阅 ---------- */
function renderSub(files){
  var h='';
  files.forEach(function(f,i){
    h+='<div class="tplrow"><div class="row"><b>'+esc(f.file_name)+'</b><span class="chip">'+esc(f.kind)+'</span><span class="chip">'+f.count+' 节点</span><span class="chip">'+(f.encoded?'base64':'明文')+'</span>'
      +(f.written?'<span class="pill ok">已写入</span>':'<span class="pill">预览</span>')
      +'<span class="muted">'+esc(f.path)+'</span><div class="spacer"></div>'
      +'<button class="mini" onclick="copySub('+i+')">复制</button>'
      +'<a href="'+U('/sub/')+encodeURIComponent(f.file_name)+'" target="_blank">打开</a></div>'
      +(f.error?('<div class="help" style="color:#f87171">'+esc(f.error)+'</div>'):'')
      +'<textarea id="sub_ta_'+i+'" rows="6" spellcheck="false">'+esc((f.nodes||[]).join('\n'))+'</textarea></div>';
  });
  $('subbox').innerHTML=h||'<div class="muted">无输出</div>';
}
function previewSub(){api(U('/api/sub/preview'),'POST',{}).then(function(d){renderSub(d.files);toast('预览完成','ok');}).catch(function(e){toast(e.message,'err');});}
function genSub(){api(U('/api/sub/generate'),'POST',{}).then(function(d){renderSub(d.files);toast('订阅已生成','ok');loadFiles();}).catch(function(e){toast(e.message,'err');});}
function copySub(i){
  var ta=$('sub_ta_'+i);ta.select();
  if(navigator.clipboard&&navigator.clipboard.writeText){navigator.clipboard.writeText(ta.value).then(function(){toast('已复制','ok');},function(){document.execCommand('copy');toast('已复制','ok');});}
  else{document.execCommand('copy');toast('已复制','ok');}
}
/* ---------- DNS ---------- */
function dnsPlan(){
  api(U('/api/dns/plan'),'POST',{}).then(function(d){
    var h='<table><thead><tr><th>#</th><th>域名</th><th>类型</th><th>内容</th><th>TTL</th><th>Proxied</th><th>国家</th></tr></thead><tbody>';
    (d.items||[]).forEach(function(it){h+='<tr><td>'+it.index+'</td><td>'+esc(it.name)+'</td><td>'+esc(it.type)+'</td><td>'+esc(it.content)+'</td><td>'+it.ttl+'</td><td>'+(it.proxied?'是':'否')+'</td><td>'+esc(it.country)+'</td></tr>';});
    $('dnsplanbox').innerHTML=h+'</tbody></table>';
    toast('计划 '+d.count+' 条'+(d.enabled?'':' (推送未开启)'),'ok');
  }).catch(function(e){toast(e.message,'err');});
}
function dnsRecords(){
  var n=$('dnsname').value;
  api(U('/api/dns/records')+(n?('?name='+encodeURIComponent(n)):'')).then(function(d){
    DNSREC=d.records||[];
    var h='<table><thead><tr><th>名称</th><th>类型</th><th>内容</th><th>TTL</th><th>Proxied</th><th>操作</th></tr></thead><tbody>';
    DNSREC.forEach(function(r,i){h+='<tr><td>'+esc(r.name)+'</td><td>'+esc(r.type)+'</td><td>'+esc(r.content)+'</td><td>'+r.ttl+'</td><td>'+(r.proxied?'是':'否')+'</td><td><button class="mini danger" onclick="delRec('+i+')">删除</button></td></tr>';});
    $('dnsrecbox').innerHTML=h+'</tbody></table>';
  }).catch(function(e){toast(e.message,'err');});
}
function delRec(i){
  var r=DNSREC[i];if(!r)return;
  if(!confirm('删除记录 '+r.name+' ?'))return;
  api(U('/api/dns/records?id=')+encodeURIComponent(r.id),'DELETE').then(function(){toast('已删除','ok');dnsRecords();}).catch(function(e){toast(e.message,'err');});
}
/* ---------- 文件(仅订阅输出目录, 用索引传参避免路径转义问题) ---------- */
function fmtSize(n){if(n<1024)return n+' B';if(n<1048576)return (n/1024).toFixed(1)+' KB';return (n/1048576).toFixed(2)+' MB';}
function loadFiles(){
  return api(U('/api/files')).then(function(d){
    FILES=d.files||[];
    $('fileinfo').textContent='订阅输出目录: '+(d.root||'')+(d.exists===false?' (目录不存在)':'')+' | '+FILES.length+' 个文件';
    var h='<table><thead><tr><th>文件</th><th>相对路径</th><th>大小</th><th>修改时间</th><th>操作</th></tr></thead><tbody>';
    FILES.forEach(function(f,i){
      h+='<tr><td>'+esc(f.name)+'</td><td class="muted">'+esc(f.rel)+'</td><td>'+fmtSize(f.size)+'</td><td>'+esc(f.mtime)+'</td><td>'
        +'<button class="mini" onclick="viewFile('+i+')">查看</button> '
        +'<a href="'+U('/api/file?download=1&path=')+encodeURIComponent(f.path)+'">下载</a> '
        +'<a href="'+U('/sub/')+encodeURIComponent(f.name)+'" target="_blank">订阅链接</a></td></tr>';
    });
    $('filesbox').innerHTML=h+'</tbody></table>';
  }).catch(function(e){toast(e.message,'err');});
}
function viewFile(i){
  var f=FILES[i];if(!f)return;
  if(f.ext==='.csv'){showTab('res');loadResults(f.path);return;}
  fetch(U('/api/file?path=')+encodeURIComponent(f.path)).then(function(r){return r.text();}).then(function(t){
    VIEWTEXT=t;$('viewname').textContent=f.rel;$('fileview').textContent=t.slice(0,200000);
  }).catch(function(e){toast(e.message,'err');});
}
function decodeView(){
  if(!VIEWTEXT){toast('先查看一个文件','err');return;}
  try{
    var bin=atob(VIEWTEXT.replace(/\s/g,''));
    var bytes=new Uint8Array(bin.length);
    for(var i=0;i<bin.length;i++)bytes[i]=bin.charCodeAt(i);
    $('fileview').textContent=new TextDecoder('utf-8').decode(bytes);
    toast('已解码','ok');
  }catch(e){toast('Base64 解码失败','err');}
}
/* ---------- 工具 ---------- */
function previewSources(){
  var src=$('srcinput').value.split('\n').map(function(s){return s.trim();}).filter(function(s){return s;});
  $('srcout').textContent='解析中...';
  api(U('/api/sources/preview'),'POST',{sources:src,depth:parseInt($('srcdepth').value,10)||0}).then(function(d){
    $('srcout').textContent='唯一 IP: '+d.count+' (最多展示 1000)\n\n'+(d.ips||[]).join('\n');
  }).catch(function(e){$('srcout').textContent='失败: '+e.message;});
}
function testProxy(){
  $('proxyout').textContent='测试中...';
  api(U('/api/proxy/test'),'POST',{proxy:$('proxyinput').value,apply:$('proxyapply').checked}).then(function(d){$('proxyout').textContent=JSON.stringify(d,null,2);}).catch(function(e){$('proxyout').textContent='失败: '+e.message;});
}
function testIP(){
  $('ipout').textContent='测试中...';
  api(U('/api/ip/test'),'POST',{ip:$('tip').value,port:parseInt($('tport').value,10)||443,speed:$('tspeed').checked}).then(function(d){$('ipout').textContent=JSON.stringify(d,null,2);}).catch(function(e){$('ipout').textContent='失败: '+e.message;});
}
/* ---------- 初始化 ---------- */
window.addEventListener('DOMContentLoaded',function(){
  buildConfigForm();
  var form=$('cfgform');
  form.addEventListener('input',refreshDirty);
  form.addEventListener('change',refreshDirty);
  var bs=document.querySelectorAll('nav button');
  for(var i=0;i<bs.length;i++){(function(b){b.onclick=function(){switchTab(b.getAttribute('data-tab'));};})(bs[i]);}
  loadConfig(false).then(function(){return loadState();});
  connectSSE();
  loadResults();
  setInterval(function(){loadState();},5000);
});
window.addEventListener('beforeunload',function(e){if(isDirty()){e.preventDefault();e.returnValue='';}});
</script>
</body>
</html>`