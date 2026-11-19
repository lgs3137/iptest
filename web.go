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
	"bytes"
	"os/exec"
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
	"/api/state":             handleState,
	"/api/config":            handleConfig,
	"/api/config/reset":      handleConfigReset,
	"/api/run":               handleRun,
	"/api/stop":              handleStop,
	"/api/logs":              handleLogs,
	"/api/events":            handleEvents,
	"/api/results":           handleResults,
	"/api/history":           handleHistory,
	"/api/history/trim":      handleHistoryTrim,
	"/api/history/clear":     handleHistoryClear,
	"/api/files":             handleFiles,
	"/api/file":              handleFile,
	"/api/sources/preview":   handleSourcesPreview,
	"/api/proxy/test":        handleProxyTest,
	"/api/ip/test":           handleIPTest,
	"/api/sub/preview":       handleSubPreview,
	"/api/sub/generate":      handleSubGenerate,
	"/api/dns/plan":          handleDNSPlan,
	"/api/dns/records":       handleDNSRecords,
	"/api/dns/record":        handleDNSRecordPush,
	"/api/schedule":          handleSchedule,
	"/api/schedule/run":      handleScheduleRun,
	"/api/schedule/preview":  handleSchedulePreview,
	"/api/schedule/snapshot": handleScheduleSnapshot,
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
		stopScheduler()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	go initProxy(cfg.Proxy)
	startScheduler()

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
		if cfg.Schedule == nil {
			cfg.Schedule = currentConfig().Schedule
		}
		normalizeConfig(&cfg)
		old := currentConfig()
		if err := saveConfigToFile(&cfg, configPath()); err != nil {
			errJSON(w, 500, "保存配置失败: "+err.Error())
			return
		}
		setGlobalConfig(&cfg)
		reloadSchedule()
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

func handleDNSRecordPush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errJSON(w, 405, "方法不允许")
		return
	}
	var req struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Content  string `json:"content"`
		TTL      int    `json:"ttl"`
		Proxied  bool   `json:"proxied"`
		Priority *int   `json:"priority"`
	}
	if err := readJSON(r, &req); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	rec := CloudflareDNSRecord{
		Type:     strings.ToUpper(strings.TrimSpace(req.Type)),
		Name:     strings.TrimSpace(req.Name),
		Content:  strings.TrimSpace(req.Content),
		TTL:      req.TTL,
		Proxied:  req.Proxied,
		Priority: req.Priority,
	}
	if rec.Type == "" || rec.Name == "" || rec.Content == "" {
		errJSON(w, 400, "type / name / content 均不能为空")
		return
	}
	switch rec.Type {
	case "A", "AAAA", "CNAME", "TXT", "MX", "NS", "SRV", "CAA", "PTR", "TLSA", "SSHFP", "DS", "DNSKEY", "URI", "LOC", "CERT":
	default:
		errJSON(w, 400, "不支持的记录类型: "+rec.Type)
		return
	}
	if rec.TTL <= 0 {
		rec.TTL = 300
	}
	if rec.Proxied && rec.Type != "A" && rec.Type != "AAAA" && rec.Type != "CNAME" {
		logWarnf("[dns] %s 记录不支持 proxied, 已自动置为 false", rec.Type)
		rec.Proxied = false
	}
	cfg := currentConfig()
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	if err := upsertCFRecord(ctx, cfg.DNSConfig, rec); err != nil {
		logErrf("[dns] 手动推送失败 [%s (%s) -> %s]: %v", rec.Name, rec.Type, rec.Content, err)
		errJSON(w, 400, err.Error())
		return
	}
	logOkf("[dns] 手动推送/更新成功: %s (%s) -> %s (ttl=%d, proxied=%v)", rec.Name, rec.Type, rec.Content, rec.TTL, rec.Proxied)
	writeJSON(w, 200, map[string]any{"ok": true, "record": rec})
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

type ScheduleConfig struct {
	Enabled bool            `json:"enabled"`
	Tasks   []ScheduledTask `json:"tasks"`
}

type ScheduledTask struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Enabled bool      `json:"enabled"`
	Cron    string    `json:"cron"`
	Mode    string    `json:"mode"`
	Shell   string    `json:"shell"`
	Script  string    `json:"script"`
	Timeout int       `json:"timeout_sec"`
	Guard   TaskGuard `json:"guard"`
}

type TaskGuard struct {
	TopSpeedEnabled bool    `json:"top_speed_enabled"`
	TopSpeedMBps    float64 `json:"top_speed_mbps"`
	TopIP           string  `json:"top_ip"`
	TopPort         int     `json:"top_port"`
	SourceChanged   bool    `json:"source_changed_enabled"`
}

func normalizeSchedule(sc *ScheduleConfig) {
	if sc == nil {
		return
	}
	seen := map[string]bool{}
	for i := range sc.Tasks {
		t := &sc.Tasks[i]
		t.Name = strings.TrimSpace(t.Name)
		if t.Name == "" {
			t.Name = fmt.Sprintf("任务%d", i+1)
		}
		t.ID = strings.TrimSpace(t.ID)
		if t.ID == "" {
			t.ID = fmt.Sprintf("task-%d", i+1)
		}
		for seen[t.ID] {
			t.ID += "x"
		}
		seen[t.ID] = true
		t.Cron = strings.TrimSpace(t.Cron)
		if t.Cron == "" {
			t.Cron = "0 4 * * *"
		}
		t.Mode = strings.ToLower(strings.TrimSpace(t.Mode))
		switch t.Mode {
		case "full", "speedtest", "sub", "dns", "script":
		default:
			t.Mode = "full"
		}
		if t.Timeout < 0 {
			t.Timeout = 0
		}
		if t.Guard.TopSpeedMBps < 0 {
			t.Guard.TopSpeedMBps = 0
		}
		if t.Guard.TopPort < 0 || t.Guard.TopPort > 65535 {
			t.Guard.TopPort = 0
		}
		t.Shell = strings.TrimSpace(t.Shell)
	}
}

type cronSpec struct {
	every                    time.Duration
	min, hour, dom, mon, dow uint64
	domStar, dowStar         bool
}

var cronMonthNames = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
var cronDowNames = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

func cronNum(s string, names map[string]int) (int, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if names != nil {
		if v, ok := names[s]; ok {
			return v, nil
		}
	}
	return strconv.Atoi(s)
}

func parseCronField(field string, lo, hi int, names map[string]int) (uint64, bool, error) {
	var mask uint64
	star := strings.HasPrefix(field, "*")
	for _, part := range strings.Split(field, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return 0, false, errors.New("存在空字段")
		}
		step := 1
		if i := strings.Index(part, "/"); i >= 0 {
			v, err := strconv.Atoi(part[i+1:])
			if err != nil || v <= 0 {
				return 0, false, fmt.Errorf("步长无效: %s", part)
			}
			step, part = v, part[:i]
		}
		var a, b int
		var err error
		switch {
		case part == "*":
			a, b = lo, hi
		case strings.Contains(part, "-"):
			r := strings.SplitN(part, "-", 2)
			if a, err = cronNum(r[0], names); err != nil {
				return 0, false, fmt.Errorf("无效取值: %s", part)
			}
			if b, err = cronNum(r[1], names); err != nil {
				return 0, false, fmt.Errorf("无效取值: %s", part)
			}
		default:
			if a, err = cronNum(part, names); err != nil {
				return 0, false, fmt.Errorf("无效取值: %s", part)
			}
			b = a
			if step > 1 {
				b = hi
			}
		}
		if a < lo || b > hi || a > b {
			return 0, false, fmt.Errorf("取值超出范围 [%d,%d]: %s", lo, hi, part)
		}
		for v := a; v <= b; v += step {
			mask |= 1 << uint(v)
		}
	}
	return mask, star, nil
}

func parseCron(expr string) (*cronSpec, error) {
	s := strings.ToLower(strings.TrimSpace(expr))
	if s == "" {
		return nil, errors.New("cron 表达式为空")
	}
	switch s {
	case "@hourly":
		s = "0 * * * *"
	case "@daily", "@midnight":
		s = "0 0 * * *"
	case "@weekly":
		s = "0 0 * * 0"
	case "@monthly":
		s = "0 0 1 * *"
	case "@yearly", "@annually":
		s = "0 0 1 1 *"
	}
	if strings.HasPrefix(s, "@every ") {
		d, err := time.ParseDuration(strings.TrimSpace(s[7:]))
		if err != nil || d < 10*time.Second {
			return nil, errors.New("@every 间隔无效(最小 10s), 例: @every 30m")
		}
		return &cronSpec{every: d}, nil
	}
	f := strings.Fields(s)
	if len(f) != 5 {
		return nil, fmt.Errorf("需要 5 个字段(分 时 日 月 周), 当前 %d 个", len(f))
	}
	var sp cronSpec
	var err error
	if sp.min, _, err = parseCronField(f[0], 0, 59, nil); err != nil {
		return nil, fmt.Errorf("分钟: %v", err)
	}
	if sp.hour, _, err = parseCronField(f[1], 0, 23, nil); err != nil {
		return nil, fmt.Errorf("小时: %v", err)
	}
	if sp.dom, sp.domStar, err = parseCronField(f[2], 1, 31, nil); err != nil {
		return nil, fmt.Errorf("日: %v", err)
	}
	if sp.mon, _, err = parseCronField(f[3], 1, 12, cronMonthNames); err != nil {
		return nil, fmt.Errorf("月: %v", err)
	}
	if sp.dow, sp.dowStar, err = parseCronField(f[4], 0, 7, cronDowNames); err != nil {
		return nil, fmt.Errorf("周: %v", err)
	}
	if sp.dow&(1<<7) != 0 {
		sp.dow |= 1
		sp.dow &^= 1 << 7
	}
	return &sp, nil
}

func (s *cronSpec) dayMatch(t time.Time) bool {
	domOK := s.dom&(1<<uint(t.Day())) != 0
	dowOK := s.dow&(1<<uint(t.Weekday())) != 0
	switch {
	case s.domStar && s.dowStar:
		return true
	case s.domStar:
		return dowOK
	case s.dowStar:
		return domOK
	}
	return domOK || dowOK
}

func (s *cronSpec) next(from time.Time) time.Time {
	if s.every > 0 {
		return from.Add(s.every)
	}
	loc := from.Location()
	t := time.Date(from.Year(), from.Month(), from.Day(), from.Hour(), from.Minute(), 0, 0, loc).Add(time.Minute)
	limit := t.AddDate(1, 0, 0)
	for t.Before(limit) {
		if s.mon&(1<<uint(t.Month())) == 0 {
			t = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, loc).AddDate(0, 1, 0)
			continue
		}
		if !s.dayMatch(t) {
			t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1)
			continue
		}
		if s.hour&(1<<uint(t.Hour())) == 0 {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, loc).Add(time.Hour)
			continue
		}
		if s.min&(1<<uint(t.Minute())) == 0 {
			t = t.Add(time.Minute)
			continue
		}
		return t
	}
	return time.Time{}
}

func nextRunAfter(t ScheduledTask, from time.Time) time.Time {
	if !t.Enabled {
		return time.Time{}
	}
	sp, err := parseCron(t.Cron)
	if err != nil {
		return time.Time{}
	}
	return sp.next(from)
}

type sourceSnapshot struct {
	Time string   `json:"time"`
	IPs  []string `json:"ips"`
}

var (
	srcMu   sync.Mutex
	srcSnap sourceSnapshot
)

func sourceSnapshotPath() string {
	return filepath.Join(os.TempDir(), "iptest-source-snapshot.json")
}

func loadSourceSnapshot() {
	data, err := os.ReadFile(sourceSnapshotPath())
	if err != nil {
		return
	}
	var s sourceSnapshot
	if json.Unmarshal(data, &s) == nil && len(s.IPs) > 0 {
		srcMu.Lock()
		srcSnap = s
		srcMu.Unlock()
		logf("[sched] 已载入输入源快照: %d 个 IP (%s)", len(s.IPs), s.Time)
	}
}

func sourceSnapshotIPs() []string {
	srcMu.Lock()
	defer srcMu.Unlock()
	return append([]string(nil), srcSnap.IPs...)
}

func noteSourceIPs(ips []string) {
	if len(ips) == 0 {
		return
	}
	srcMu.Lock()
	same := len(srcSnap.IPs) == len(ips)
	if same {
		for i := range ips {
			if srcSnap.IPs[i] != ips[i] {
				same = false
				break
			}
		}
	}
	srcSnap = sourceSnapshot{Time: time.Now().Format("2006-01-02 15:04:05"), IPs: append([]string(nil), ips...)}
	s := srcSnap
	srcMu.Unlock()
	if same {
		return
	}
	if b, err := json.Marshal(s); err == nil {
		_ = os.WriteFile(sourceSnapshotPath(), b, 0644)
	}
}

type taskState struct {
	Next    time.Time
	Last    time.Time
	Status  string
	Msg     string
	Runs    int
	Skips   int
	Running bool
}

var (
	schedMu       sync.Mutex
	schedStates   = map[string]*taskState{}
	schedStop     chan struct{}
	schedStartOne sync.Once
	schedStopOne  sync.Once
)

func jobDoneChan() chan struct{} {
	jobMu.Lock()
	defer jobMu.Unlock()
	return jobDone
}

func setTaskResult(id, status, msg string) {
	schedMu.Lock()
	s := schedStates[id]
	if s == nil {
		s = &taskState{}
		schedStates[id] = s
	}
	s.Status, s.Msg, s.Last = status, msg, time.Now()
	if status == "skipped" {
		s.Skips++
	} else {
		s.Runs++
	}
	schedMu.Unlock()
}

func currentTaskByID(id string) *ScheduledTask {
	sc := currentConfig().Schedule
	if sc == nil {
		return nil
	}
	for i := range sc.Tasks {
		if sc.Tasks[i].ID == id {
			return &sc.Tasks[i]
		}
	}
	return nil
}

func reloadSchedule() {
	sc := currentConfig().Schedule
	now := time.Now()
	type line struct{ name, cron, mode, next string; enabled bool }
	var lines []line

	schedMu.Lock()
	old := schedStates
	schedStates = make(map[string]*taskState)
	if sc != nil {
		for _, t := range sc.Tasks {
			s := &taskState{}
			if o := old[t.ID]; o != nil {
				s.Last, s.Status, s.Msg = o.Last, o.Status, o.Msg
				s.Runs, s.Skips, s.Running = o.Runs, o.Skips, o.Running
				if s.Running {
					s.Next = o.Next
				}
			}
			if !s.Running {
				s.Next = nextRunAfter(t, now)
			}
			schedStates[t.ID] = s
			nx := fmtTime(s.Next)
			if nx == "" {
				nx = "-"
			}
			lines = append(lines, line{t.Name, t.Cron, t.Mode, nx, t.Enabled})
		}
	}
	enabled := sc != nil && sc.Enabled
	schedMu.Unlock()

	logOkf("[sched] 计划已重新初始化: 共 %d 个任务, 启用调度=%v", len(lines), enabled)
	for _, l := range lines {
		if l.enabled {
			logf("[sched]   · %s | %s | %s | 下次 %s", l.name, l.cron, l.mode, l.next)
		} else {
			logf("[sched]   · %s | %s | %s | 已停用", l.name, l.cron, l.mode)
		}
	}
}

func startScheduler() {
	schedStartOne.Do(func() {
		loadSourceSnapshot()
		schedStop = make(chan struct{})
		reloadSchedule()
		go func() {
			tk := time.NewTicker(5 * time.Second)
			defer tk.Stop()
			for {
				select {
				case <-schedStop:
					return
				case <-tk.C:
					schedTick()
				}
			}
		}()
		zone, _ := time.Now().Zone()
		logf("[sched] 计划任务调度器已启动 (随 Web 服务运行, 时区 %s, 精度 ~5s)", zone)
	})
}

func stopScheduler() {
	schedStopOne.Do(func() {
		if schedStop != nil {
			close(schedStop)
		}
	})
}

func schedTick() {
	sc := currentConfig().Schedule
	if sc == nil || !sc.Enabled {
		return
	}
	now := time.Now()
	var due []ScheduledTask
	schedMu.Lock()
	for _, t := range sc.Tasks {
		if !t.Enabled {
			continue
		}
		s := schedStates[t.ID]
		if s == nil {
			s = &taskState{}
			schedStates[t.ID] = s
		}
		if s.Running {
			continue
		}
		if s.Next.IsZero() {
			s.Next = nextRunAfter(t, now)
			continue
		}
		if now.Before(s.Next) {
			continue
		}
		s.Next = nextRunAfter(t, now)
		s.Running, s.Status, s.Last = true, "running", now
		due = append(due, t)
	}
	schedMu.Unlock()
	for _, t := range due {
		go execScheduledTask(t, false, false, false)
	}
}

func evaluateGuards(t ScheduledTask, st Settings) (bool, string) {
	g := t.Guard
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	if g.TopSpeedEnabled {
		ip, port := strings.TrimSpace(g.TopIP), g.TopPort
		if ip == "" {
			results, err := loadSpeedResultsFromFile(st.OutFile)
			if err != nil || len(results) == 0 {
				logWarnf("[task:%s] 条件①: 无可用历史结果, 视为需要执行", t.Name)
			} else {
				ip = results[0].IP
				if port <= 0 {
					port, _ = strconv.Atoi(results[0].Port)
				}
			}
		}
		if ip != "" {
			if port <= 0 {
				port = 443
			}
			sp := getDownloadSpeed(ctx, st, ip, port)
			logf("[task:%s] 条件①: 榜首复测 %s:%d → %.2f MB/s (阈值 %.2f)", t.Name, ip, port, sp, g.TopSpeedMBps)
			if sp > 0 && sp >= g.TopSpeedMBps {
				return true, fmt.Sprintf("榜首 %s 仍有 %.2f MB/s ≥ %.2f, 无需重测", ip, sp, g.TopSpeedMBps)
			}
		}
	}

	if g.SourceChanged {
		old := sourceSnapshotIPs()
		setPhase("guard", "计划预检: 采集输入源", 0)
		ips, err := fetchIPsFromSources(ctx, st, st.Sources, st.SourceDepth)
		if err != nil {
			logWarnf("[task:%s] 条件②: 预采集失败(%v), 视为需要执行", t.Name, err)
			return false, ""
		}
		if len(old) == 0 {
			logf("[task:%s] 条件②: 无历史快照, 视为需要执行 (本次 %d 个 IP)", t.Name, len(ips))
		} else {
			set := make(map[string]struct{}, len(old))
			for _, v := range old {
				set[v] = struct{}{}
			}
			newCnt := 0
			for _, v := range ips {
				if _, ok := set[v]; !ok {
					newCnt++
				}
			}
			logf("[task:%s] 条件②: 本次 %d / 上次 %d / 新增 %d", t.Name, len(ips), len(old), newCnt)
			if len(ips) > 0 && newCnt == 0 {
				return true, fmt.Sprintf("输入源无新增 IP (%d 个全部命中上次记录)", len(ips))
			}
		}
	}
	return false, ""
}

type logWriter struct {
	prefix string
	level  string
	buf    []byte
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(w.buf[:i]), "\r")
		w.buf = w.buf[i+1:]
		if strings.TrimSpace(line) != "" {
			hub.add(w.level, w.prefix+line)
		}
	}
	if len(w.buf) > 64*1024 {
		w.flush()
	}
	return len(p), nil
}

func (w *logWriter) flush() {
	if len(w.buf) > 0 {
		hub.add(w.level, w.prefix+string(w.buf))
		w.buf = nil
	}
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			return false
		}
	}
	return true
}

func runScriptTask(t ScheduledTask, cfg *AppConfig, st Settings) {
	script := strings.ReplaceAll(strings.TrimSpace(t.Script), "\r\n", "\n")
	if script == "" {
		logErrf("[task:%s] 脚本内容为空", t.Name)
		setTaskResult(t.ID, "error", "脚本内容为空")
		return
	}
	cwd, _ := os.Getwd()
	shell := strings.TrimSpace(t.Shell)
	if shell == "" {
		if runtime.GOOS == "windows" {
			shell = "cmd"
		} else {
			shell = "sh"
		}
	}
	base := strings.ToLower(filepath.Base(shell))
	kind := "posix"
	switch {
	case base == "cmd" || base == "cmd.exe":
		kind = "cmd"
	case strings.Contains(base, "powershell") || base == "pwsh" || base == "pwsh.exe":
		kind = "ps"
	}
	ext := ".sh"
	if kind == "cmd" {
		ext = ".bat"
	} else if kind == "ps" {
		ext = ".ps1"
	}

	f, err := os.CreateTemp("", "iptest-task-*"+ext)
	if err != nil {
		logErrf("[task:%s] 创建临时脚本失败: %v", t.Name, err)
		setTaskResult(t.ID, "error", err.Error())
		return
	}
	path := f.Name()
	defer os.Remove(path)

	body := script + "\n"
	switch kind {
	case "cmd":
		var b strings.Builder
		b.WriteString("@echo off\r\n")
		if !isASCII(script) {
			b.WriteString("chcp 65001 >nul 2>&1\r\n")
		}
		b.WriteString("cd /d \"" + cwd + "\"\r\n")
		b.WriteString(strings.ReplaceAll(script, "\n", "\r\n"))
		b.WriteString("\r\n")
		body = b.String()
	case "ps":
		body = "\ufeff" + script + "\n"
	}
	if _, err := f.WriteString(body); err != nil {
		f.Close()
		logErrf("[task:%s] 写入临时脚本失败: %v", t.Name, err)
		setTaskResult(t.ID, "error", err.Error())
		return
	}
	f.Close()

	timeout := time.Duration(t.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	exe, _ := os.Executable()
	var cmd *exec.Cmd
	switch kind {
	case "cmd":
		cmd = exec.CommandContext(ctx, shell, "/D", "/C", filepath.Base(path))
		cmd.Dir = filepath.Dir(path)
	case "ps":
		cmd = exec.CommandContext(ctx, shell, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", path)
		cmd.Dir = cwd
	default:
		cmd = exec.CommandContext(ctx, shell, path)
		cmd.Dir = cwd
	}
	cmd.Env = append(os.Environ(),
		"IPTEST_CONFIG="+configPath(),
		"IPTEST_OUTFILE="+st.OutFile,
		"IPTEST_SUBDIR="+cfg.SubConfig.OutputDir,
		"IPTEST_BIN="+exe,
		"IPTEST_TASK="+t.Name,
	)
	prefix := "[task:" + t.Name + "] "
	wo := &logWriter{prefix: prefix, level: "info"}
	we := &logWriter{prefix: prefix, level: "warn"}
	cmd.Stdout, cmd.Stderr = wo, we

	logf("[task:%s] 开始执行脚本 (shell=%s, 超时 %ds, 工作目录 %s)", t.Name, shell, int(timeout.Seconds()), cwd)
	logDebugf("[task:%s] 脚本内容:\n%s", t.Name, script)
	start := time.Now()
	runErr := cmd.Run()
	wo.flush()
	we.flush()
	el := time.Since(start).Seconds()

	code := 0
	if ee, ok := runErr.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		logErrf("[task:%s] 脚本超时被终止 (%.1fs)", t.Name, el)
		setTaskResult(t.ID, "error", fmt.Sprintf("脚本超时 %ds", int(timeout.Seconds())))
	case runErr != nil:
		logErrf("[task:%s] 脚本失败 (退出码 %d, %.1fs): %v", t.Name, code, el, runErr)
		setTaskResult(t.ID, "error", fmt.Sprintf("退出码 %d: %v", code, runErr))
	default:
		logOkf("[task:%s] 脚本完成 (退出码 0, 耗时 %.1fs)", t.Name, el)
		setTaskResult(t.ID, "ok", fmt.Sprintf("退出码 0, 耗时 %.1fs", el))
	}
}

func execScheduledTask(t ScheduledTask, manual, guardOnly, ignoreGuard bool) {
	defer func() {
		cur := currentTaskByID(t.ID)
		schedMu.Lock()
		if s := schedStates[t.ID]; s != nil {
			s.Running = false
			if cur != nil {
				s.Next = nextRunAfter(*cur, time.Now())
			} else {
				s.Next = time.Time{}
			}
		}
		schedMu.Unlock()
	}()
	defer func() {
		if e := recover(); e != nil {
			logErrf("[task:%s] 执行异常: %v", t.Name, e)
			setTaskResult(t.ID, "error", fmt.Sprint(e))
		}
	}()

	trigger := "定时"
	if manual {
		trigger = "手动"
	}
	start := time.Now()
	logf("[task:%s] ===== 触发(%s) 内容=%s =====", t.Name, trigger, t.Mode)
	cfg := currentConfig()
	st := settingsFromConfig(cfg)

	if t.Mode == "script" {
		runScriptTask(t, cfg, st)
		return
	}
	if isRunning() {
		logWarnf("[task:%s] 已有任务运行中, 跳过本次", t.Name)
		setTaskResult(t.ID, "skipped", "已有任务运行中")
		return
	}
	if !ignoreGuard && (t.Guard.TopSpeedEnabled || t.Guard.SourceChanged) {
		skip, reason := evaluateGuards(t, st)
		setPhase("idle", "空闲", 0)
		if skip {
			logOkf("[task:%s] 前置条件命中, 跳过执行: %s", t.Name, reason)
			setTaskResult(t.ID, "skipped", reason)
			return
		}
		logf("[task:%s] 前置条件未命中, 继续执行", t.Name)
	}
	if guardOnly {
		logOkf("[task:%s] 仅预检完成: 可以执行(未启动任务)", t.Name)
		setTaskResult(t.ID, "ok", "预检通过(未执行)")
		return
	}
	if _, err := startJob(t.Mode, cfg); err != nil {
		logWarnf("[task:%s] 启动失败: %v", t.Name, err)
		setTaskResult(t.ID, "skipped", err.Error())
		return
	}
	if done := jobDoneChan(); done != nil {
		if t.Timeout > 0 {
			tm := time.NewTimer(time.Duration(t.Timeout) * time.Second)
			select {
			case <-done:
				tm.Stop()
			case <-tm.C:
				logWarnf("[task:%s] 超过 %ds 未完成, 请求停止", t.Name, t.Timeout)
				stopJob()
				<-done
			}
		} else {
			<-done
		}
	}
	v := jobSnapshot()
	msg := fmt.Sprintf("有效IP %d / 结果 %d 行 / 耗时 %.1fs", v.Stats.Valid, v.Stats.ResultRows, time.Since(start).Seconds())
	switch v.Phase {
	case "done":
		logOkf("[task:%s] 完成: %s", t.Name, msg)
		setTaskResult(t.ID, "ok", msg)
	case "stopped":
		logWarnf("[task:%s] 已停止: %s", t.Name, msg)
		setTaskResult(t.ID, "skipped", "任务被停止")
	default:
		logErrf("[task:%s] 失败: %s", t.Name, v.Error)
		setTaskResult(t.ID, "error", v.Error)
	}
}

func handleSchedule(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		sc := currentConfig().Schedule
		type taskView struct {
			ScheduledTask
			Next    string `json:"next"`
			Last    string `json:"last"`
			Status  string `json:"status"`
			Msg     string `json:"msg"`
			Runs    int    `json:"runs"`
			Skips   int    `json:"skips"`
			Running bool   `json:"running"`
		}
		out := []taskView{}
		schedMu.Lock()
		if sc != nil {
			for _, t := range sc.Tasks {
				v := taskView{ScheduledTask: t}
				if s := schedStates[t.ID]; s != nil {
					v.Next, v.Last = fmtTime(s.Next), fmtTime(s.Last)
					v.Status, v.Msg = s.Status, s.Msg
					v.Runs, v.Skips, v.Running = s.Runs, s.Skips, s.Running
				}
				out = append(out, v)
			}
		}
		schedMu.Unlock()
		zone, _ := time.Now().Zone()
		writeJSON(w, 200, map[string]any{
			"ok": true, "enabled": sc != nil && sc.Enabled, "tasks": out,
			"now": time.Now().Format("2006-01-02 15:04:05"), "tz": zone,
			"job_running": isRunning(),
		})
	case http.MethodPost:
		var sc ScheduleConfig
		if err := readJSON(r, &sc); err != nil {
			errJSON(w, 400, err.Error())
			return
		}
		normalizeSchedule(&sc)
		for _, t := range sc.Tasks {
			if _, err := parseCron(t.Cron); err != nil {
				errJSON(w, 400, fmt.Sprintf("任务[%s] cron 无效: %v", t.Name, err))
				return
			}
			if t.Mode == "script" && strings.TrimSpace(t.Script) == "" {
				errJSON(w, 400, fmt.Sprintf("任务[%s] 脚本内容为空", t.Name))
				return
			}
		}
		cfg := currentConfig()
		cfg.Schedule = &sc
		if err := saveConfigToFile(cfg, configPath()); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		setGlobalConfig(cfg)
		reloadSchedule()
		logOkf("[sched] 计划任务已保存: %d 个任务, 总开关=%v", len(sc.Tasks), sc.Enabled)
		writeJSON(w, 200, map[string]any{"ok": true})
	default:
		errJSON(w, 405, "方法不允许")
	}
}

func handleScheduleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errJSON(w, 405, "方法不允许")
		return
	}
	var req struct {
		ID          string `json:"id"`
		GuardOnly   bool   `json:"guard_only"`
		IgnoreGuard bool   `json:"ignore_guard"`
	}
	if err := readJSON(r, &req); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	sc := currentConfig().Schedule
	var task *ScheduledTask
	if sc != nil {
		for i := range sc.Tasks {
			if sc.Tasks[i].ID == req.ID {
				task = &sc.Tasks[i]
				break
			}
		}
	}
	if task == nil {
		errJSON(w, 404, "任务不存在")
		return
	}
	schedMu.Lock()
	s := schedStates[task.ID]
	if s == nil {
		s = &taskState{}
		schedStates[task.ID] = s
	}
	if s.Running {
		schedMu.Unlock()
		errJSON(w, 409, "该任务正在执行中")
		return
	}
	s.Running, s.Status, s.Last = true, "running", time.Now()
	schedMu.Unlock()
	go execScheduledTask(*task, true, req.GuardOnly, req.IgnoreGuard)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func handleSchedulePreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errJSON(w, 405, "方法不允许")
		return
	}
	var req struct {
		Cron  string `json:"cron"`
		Count int    `json:"count"`
	}
	_ = readJSON(r, &req)
	sp, err := parseCron(req.Cron)
	if err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	n := req.Count
	if n <= 0 || n > 20 {
		n = 5
	}
	var times []string
	t := time.Now()
	for i := 0; i < n; i++ {
		t = sp.next(t)
		if t.IsZero() {
			break
		}
		times = append(times, t.Format("2006-01-02 15:04:05 Mon"))
	}
	writeJSON(w, 200, map[string]any{"ok": true, "times": times})
}

func handleScheduleSnapshot(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		srcMu.Lock()
		s := srcSnap
		srcMu.Unlock()
		sample := s.IPs
		if len(sample) > 50 {
			sample = sample[:50]
		}
		writeJSON(w, 200, map[string]any{"ok": true, "time": s.Time, "count": len(s.IPs), "sample": sample, "path": sourceSnapshotPath()})
	case http.MethodDelete:
		srcMu.Lock()
		srcSnap = sourceSnapshot{}
		srcMu.Unlock()
		_ = os.Remove(sourceSnapshotPath())
		logWarnf("[sched] 输入源快照已清空")
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
  <button data-tab="sched">计划任务</button>
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
    <div class="card">
      <h3>手动推送 / 更新单条记录</h3>
      <div class="grid">
        <div class="fld"><label>类型 type</label><select id="mr_type"><option>A</option><option>AAAA</option><option>CNAME</option><option>TXT</option><option>MX</option><option>NS</option><option>SRV</option><option>CAA</option></select></div>
        <div class="fld"><label>名称 name(完整域名)</label><input type="text" id="mr_name" placeholder="a.example.com"></div>
        <div class="fld"><label>内容 content</label><input type="text" id="mr_content" placeholder="1.2.3.4 / 目标域名 / 文本"></div>
        <div class="fld"><label>TTL 秒(1=自动)</label><input type="number" id="mr_ttl" value="300"></div>
        <div class="fld"><label>Proxied(仅 A/AAAA/CNAME)</label><label class="chk"><input type="checkbox" id="mr_proxied"> 橙云</label></div>
        <div class="fld"><label>优先级 priority(可选, MX/SRV)</label><input type="number" id="mr_priority" placeholder="留空不提交"></div>
      </div>
      <div class="row" style="margin-top:10px">
        <button class="primary" onclick="pushManualRecord()">提交推送/更新</button>
        <span id="mrmsg" class="muted"></span>
      </div>
      <div class="help">使用 dns_config 的 token / zone_id；同名同类型存在则更新，否则新建，结果只记日志。计划任务脚本可直接调用：<br>curl -s -u 用户:密码 -X POST "http://127.0.0.1:3000/前缀/api/dns/record" -H "Content-Type: application/json" -d '{"type":"A","name":"a.example.com","content":"1.2.3.4","ttl":300,"proxied":false}'</div>
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

  <section id="tab-sched">
    <div class="card">
      <div class="row">
        <label class="chk"><input type="checkbox" id="sch_enabled" onchange="schDirty()"> 启用调度</label>
        <button class="primary" onclick="saveSched()">保存计划</button>
        <button onclick="loadSched()">刷新</button>
        <button onclick="editTask(-1)">+ 新建任务</button>
        <span id="schdirty" class="pill warn" style="display:none">有未保存变更</span>
        <span id="schinfo" class="muted"></span>
      </div>
      <div class="help">调度器随 Web 服务运行，Web 不常驻则不执行；服务器本地时区，精度约 5 秒。已有任务运行中时本次触发会被跳过并记录日志。编辑后需点「保存计划」写入配置文件。</div>
      <div class="wrap"><div id="schtable"></div></div>
    </div>

    <div class="card" id="schedit" style="display:none">
      <h3 id="schedit_title">编辑任务</h3>
      <div class="grid">
        <div class="fld"><label>任务名称</label><input type="text" id="t_name"></div>
        <div class="fld"><label>启用</label><label class="chk"><input type="checkbox" id="t_enabled"> 启用</label></div>
        <div class="fld"><label>执行内容</label><select id="t_mode" onchange="onTaskMode()">
          <option value="full">完整流程(测速+订阅+DNS)</option>
          <option value="speedtest">仅测速</option>
          <option value="sub">仅生成订阅</option>
          <option value="dns">仅推送 DNS</option>
          <option value="script">Shell 脚本</option>
        </select></div>
        <div class="fld"><label>cron(分 时 日 月 周)</label><input type="text" id="t_cron" placeholder="0 4 * * *"></div>
        <div class="fld"><label>超时秒(脚本默认600; 任务0=不限)</label><input type="number" id="t_timeout" value="600"></div>
        <div class="fld" id="t_shellbox"><label>Shell(留空: sh / Windows cmd)</label><input type="text" id="t_shell" placeholder="sh / bash / powershell"></div>
      </div>
      <div class="row" style="margin-top:8px">
        <button class="mini" onclick="previewCron()">校验并预览未来5次</button>
        <span id="cronmsg" class="muted"></span>
      </div>
      <div class="fld wide" id="t_scriptbox"><label>脚本内容(可用环境变量 IPTEST_CONFIG / IPTEST_OUTFILE / IPTEST_SUBDIR / IPTEST_BIN / IPTEST_TASK)</label><textarea id="t_script" rows="6" spellcheck="false"></textarea></div>
      <div id="t_guardbox">
        <h4>前置条件(命中则跳过本次执行)</h4>
        <div class="grid">
          <div class="fld"><label>① 榜首 IP 复测</label><label class="chk"><input type="checkbox" id="g_top"> 启用</label><div class="help">取上次结果第一名(或指定 IP)单独测速一次，速度 ≥ 阈值则跳过</div></div>
          <div class="fld"><label>阈值 MB/s</label><input type="number" step="0.1" id="g_topspeed" value="5"></div>
          <div class="fld"><label>指定 IP(留空=取第一名)</label><input type="text" id="g_topip"></div>
          <div class="fld"><label>端口(0=用结果中的端口)</label><input type="number" id="g_topport" value="0"></div>
          <div class="fld"><label>② 输入源有新增才执行</label><label class="chk"><input type="checkbox" id="g_src"> 启用</label><div class="help">先预采集一遍输入源，本次 IP 全部命中上次记录(忽略顺序)则跳过</div></div>
        </div>
      </div>
      <div class="row" style="margin-top:10px">
        <button class="primary" onclick="applyTask()">确定</button>
        <button onclick="closeTask()">取消</button>
      </div>
    </div>

    <div class="card">
      <h3>输入源快照(条件②比对基准)</h3>
      <div class="row"><button onclick="loadSnapshot()">刷新</button><button class="danger" onclick="clearSnapshot()">清空快照</button><span id="snapinfo" class="muted"></span></div>
      <pre id="snapbox"></pre>
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
  if(n==='sched'){
    if(!SCHLOADED||!SCHDIRTY){loadSched();SCHLOADED=true;}
    loadSnapshot();startSchedTimer();
  }else{stopSchedTimer();}
}
function switchTab(n){
  if(CURTAB==='cfg'&&n!=='cfg'&&isDirty()){
    if(confirm('检测到配置有未保存的变更。\n\n【确定】保存配置后切换\n【取消】暂不保存直接切换(变更仍保留在表单中)')){
      saveConfig().then(function(){showTab(n);});
      return;
    }
  }
  if(CURTAB==='sched'&&n!=='sched'&&SCHDIRTY){
    if(confirm('计划任务有未保存的变更。\n\n【确定】保存后切换\n【取消】暂不保存直接切换')){saveSched();}
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
  delete c.schedule;
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
/* ---------- 计划任务(配置与运行状态分离) ---------- */
var SCHED={enabled:false,tasks:[]},SCHRT={},EDITIDX=-1,SCHDIRTY=false,SCHTIMER=null,SCHLOADED=false;
function modeText(m){return ({full:'完整流程',speedtest:'仅测速',sub:'仅订阅',dns:'仅DNS',script:'脚本'})[m]||m;}
function guardText(g){var a=[];if(g&&g.top_speed_enabled)a.push('①≥'+(g.top_speed_mbps||0)+'MB/s');if(g&&g.source_changed_enabled)a.push('②源有新增');return a.length?a.join(' + '):'无';}
function statPill(s){var c=s==='ok'?'ok':(s==='error'?'err':(s==='skipped'?'warn':(s==='running'?'run':'')));return '<span class="pill '+c+'">'+esc(s||'-')+'</span>';}
function schDirty(){SCHDIRTY=true;refreshSchDirty();}
function refreshSchDirty(){var e=$('schdirty');if(e)e.style.display=SCHDIRTY?'':'none';}
function schTask(t){var g=t.guard||{};return {id:t.id||'',name:t.name||'',enabled:!!t.enabled,cron:t.cron||'',mode:t.mode||'full',shell:t.shell||'',script:t.script||'',timeout_sec:t.timeout_sec||0,
  guard:{top_speed_enabled:!!g.top_speed_enabled,top_speed_mbps:g.top_speed_mbps||0,top_ip:g.top_ip||'',top_port:g.top_port||0,source_changed_enabled:!!g.source_changed_enabled}};}
function loadSched(){
  return api(U('/api/schedule')).then(function(d){
    SCHED={enabled:!!d.enabled,tasks:(d.tasks||[]).map(schTask)};
    SCHRT={};(d.tasks||[]).forEach(function(t){SCHRT[t.id]={next:t.next,last:t.last,status:t.status,msg:t.msg,running:t.running};});
    $('sch_enabled').checked=SCHED.enabled;
    SCHDIRTY=false;refreshSchDirty();
    $('schinfo').textContent='服务器时间 '+d.now+' ('+d.tz+') | 共 '+SCHED.tasks.length+' 个任务';
    renderSched();
  }).catch(function(e){toast('加载计划失败: '+e.message,'err');});
}
function renderSched(){
  var h='<table><thead><tr><th>#</th><th>名称</th><th>启用</th><th>cron</th><th>内容</th><th>前置条件</th><th>下次执行</th><th>上次</th><th>状态</th><th>操作</th></tr></thead><tbody>';
  SCHED.tasks.forEach(function(t,i){
    var r=SCHRT[t.id]||{};
    h+='<tr><td>'+(i+1)+'</td><td>'+esc(t.name)+'</td>'
      +'<td><input type="checkbox" '+(t.enabled?'checked':'')+' onchange="setTaskEnabled('+i+',this.checked)"></td>'
      +'<td>'+esc(t.cron)+'</td><td>'+esc(modeText(t.mode))+'</td><td class="muted">'+esc(guardText(t.guard))+'</td>'
      +'<td id="rt_next_'+i+'">'+esc(r.next||'-')+'</td>'
      +'<td id="rt_last_'+i+'" class="muted">'+esc(r.last||'-')+'</td>'
      +'<td id="rt_stat_'+i+'">'+statPill(r.status)+(r.msg?('<div class="help">'+esc(r.msg)+'</div>'):'')+'</td>'
      +'<td><button class="mini" onclick="runTask('+i+',false)">执行</button> '
      +(t.mode==='script'?'':'<button class="mini" onclick="runTask('+i+',true)">仅预检</button> ')
      +'<button class="mini" onclick="editTask('+i+')">编辑</button> '
      +'<button class="mini danger" onclick="delTask('+i+')">删除</button></td></tr>';
  });
  $('schtable').innerHTML=h+'</tbody></table>';
}
function setTaskEnabled(i,v){if(!SCHED.tasks[i])return;SCHED.tasks[i].enabled=!!v;schDirty();}
function applySchedRuntime(){
  SCHED.tasks.forEach(function(t,i){
    var r=SCHRT[t.id];if(!r)return;
    if(!t.enabled&&!r.running)return;
    var a=$('rt_next_'+i),b=$('rt_last_'+i),c=$('rt_stat_'+i);
    if(a)a.textContent=r.next||'-';
    if(b)b.textContent=r.last||'-';
    if(c)c.innerHTML=statPill(r.status)+(r.msg?('<div class="help">'+esc(r.msg)+'</div>'):'');
  });
}
function anySchedRunning(){for(var k in SCHRT){if(SCHRT[k]&&SCHRT[k].running)return true;}return false;}
function refreshSchedStatus(force){
  if(CURTAB!=='sched')return;
  if(!force&&!$('sch_enabled').checked&&!anySchedRunning())return;
  api(U('/api/schedule')).then(function(d){
    (d.tasks||[]).forEach(function(t){SCHRT[t.id]={next:t.next,last:t.last,status:t.status,msg:t.msg,running:t.running};});
    $('schinfo').textContent='服务器时间 '+d.now+' ('+d.tz+') | 共 '+SCHED.tasks.length+' 个任务'+(d.job_running?' | 有测速任务运行中':'');
    applySchedRuntime();
  }).catch(function(){});
}
function startSchedTimer(){stopSchedTimer();SCHTIMER=setInterval(function(){refreshSchedStatus(false);},10000);}
function stopSchedTimer(){if(SCHTIMER){clearInterval(SCHTIMER);SCHTIMER=null;}}
function kickSchedStatus(){[800,2500,6000,12000,20000].forEach(function(ms){setTimeout(function(){refreshSchedStatus(true);},ms);});}
function onTaskMode(){
  var s=$('t_mode').value==='script';
  $('t_scriptbox').style.display=s?'':'none';
  $('t_shellbox').style.display=s?'':'none';
  $('t_guardbox').style.display=s?'none':'';
}
function editTask(i){
  EDITIDX=i;
  var t=i<0?{name:'',enabled:true,cron:'0 4 * * *',mode:'full',timeout_sec:0,shell:'',script:'',guard:{top_speed_enabled:false,top_speed_mbps:5,top_ip:'',top_port:0,source_changed_enabled:false}}:SCHED.tasks[i];
  var g=t.guard||{};
  $('schedit_title').textContent=i<0?'新建任务':'编辑任务: '+t.name;
  $('t_name').value=t.name||'';$('t_enabled').checked=!!t.enabled;
  $('t_mode').value=t.mode||'full';$('t_cron').value=t.cron||'';
  $('t_timeout').value=t.timeout_sec||0;$('t_shell').value=t.shell||'';$('t_script').value=t.script||'';
  $('g_top').checked=!!g.top_speed_enabled;$('g_topspeed').value=g.top_speed_mbps||5;
  $('g_topip').value=g.top_ip||'';$('g_topport').value=g.top_port||0;
  $('g_src').checked=!!g.source_changed_enabled;
  $('cronmsg').textContent='';$('schedit').style.display='';onTaskMode();
}
function closeTask(){$('schedit').style.display='none';EDITIDX=-1;}
function applyTask(){
  var t={
    id:EDITIDX>=0?(SCHED.tasks[EDITIDX].id||''):'',
    name:$('t_name').value.trim(),enabled:$('t_enabled').checked,
    cron:$('t_cron').value.trim(),mode:$('t_mode').value,
    timeout_sec:parseInt($('t_timeout').value,10)||0,
    shell:$('t_shell').value.trim(),script:$('t_script').value,
    guard:{top_speed_enabled:$('g_top').checked,top_speed_mbps:parseFloat($('g_topspeed').value)||0,
           top_ip:$('g_topip').value.trim(),top_port:parseInt($('g_topport').value,10)||0,
           source_changed_enabled:$('g_src').checked}
  };
  if(EDITIDX<0)SCHED.tasks.push(t);else SCHED.tasks[EDITIDX]=t;
  closeTask();renderSched();schDirty();toast('已修改，请点「保存计划」写入配置','ok');
}
function delTask(i){if(!confirm('删除任务 '+SCHED.tasks[i].name+' ?'))return;SCHED.tasks.splice(i,1);renderSched();schDirty();}
function saveSched(){
  api(U('/api/schedule'),'POST',{enabled:$('sch_enabled').checked,tasks:SCHED.tasks})
    .then(function(){toast('计划已保存并重新初始化','ok');return loadSched();})
    .catch(function(e){toast('保存失败: '+e.message,'err');});
}
function runTask(i,guardOnly){
  var t=SCHED.tasks[i];if(!t)return;
  if(SCHDIRTY&&!confirm('有未保存的修改, 立即执行将使用【已保存】的任务定义。继续?'))return;
  api(U('/api/schedule/run'),'POST',{id:t.id,guard_only:!!guardOnly})
    .then(function(){toast(guardOnly?'已开始预检':'已开始执行','ok');kickSchedStatus();})
    .catch(function(e){toast(e.message,'err');});
}
function previewCron(){
  api(U('/api/schedule/preview'),'POST',{cron:$('t_cron').value,count:5})
    .then(function(d){$('cronmsg').textContent='未来执行: '+(d.times||[]).join(' | ');})
    .catch(function(e){$('cronmsg').textContent='表达式无效: '+e.message;});
}
function loadSnapshot(){
  api(U('/api/schedule/snapshot')).then(function(d){
    $('snapinfo').textContent=d.count+' 个 IP | 采集时间 '+(d.time||'-')+' | '+d.path;
    $('snapbox').textContent=(d.sample||[]).join('\n')+(d.count>50?'\n... 仅展示前 50 条':'');
  }).catch(function(e){toast(e.message,'err');});
}
function clearSnapshot(){
  if(!confirm('清空输入源快照? 下次条件②将视为“无历史记录”并执行'))return;
  api(U('/api/schedule/snapshot'),'DELETE').then(function(){toast('已清空','ok');loadSnapshot();}).catch(function(e){toast(e.message,'err');});
}
/* ---------- DNS 手动记录 ---------- */
function pushManualRecord(){
  var p=$('mr_priority').value.trim();
  var b={type:$('mr_type').value,name:$('mr_name').value.trim(),content:$('mr_content').value.trim(),
         ttl:parseInt($('mr_ttl').value,10)||300,proxied:$('mr_proxied').checked};
  if(!b.name||!b.content){toast('name 与 content 必填','err');return;}
  if(p!=='')b.priority=parseInt(p,10)||0;
  $('mrmsg').textContent='提交中...';
  api(U('/api/dns/record'),'POST',b).then(function(){
    $('mrmsg').textContent='成功: '+b.name+' ('+b.type+') → '+b.content;
    toast('DNS 记录已推送/更新','ok');
  }).catch(function(e){$('mrmsg').textContent='失败: '+e.message;toast(e.message,'err');});
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
});
window.addEventListener('beforeunload',function(e){if(isDirty()||SCHDIRTY){e.preventDefault();e.returnValue='';}});
</script>
</body>
</html>`