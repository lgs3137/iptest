package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	appVersion             = "3.0.0"
	defaultTCPTimeout      = 1000
	defaultProbeTimeout    = 3000
	defaultDownloadTimeout = 5000
	defaultSourceTimeout   = 20000
	defaultProbeURL        = "https://probe.609909.xyz/"
	defaultSpeedURL        = "speed.cloudflare.com/__down?bytes=20000000"
	maxSourceBytes         = 32 << 20
)

var (
	flagFile          = flag.String("file", "", "输入源列表(逗号分隔), 本地路径或 URL")
	outFile           = flag.String("outfile", "html/result.csv", "输出结果文件")
	maxThreads        = flag.Int("max", 100, "并发请求最大协程数")
	speedTest         = flag.Int("speedtest", 5, "下载测速协程数, 0 禁用测速")
	speedTestURL      = flag.String("url", defaultSpeedURL, "测速文件地址")
	delay             = flag.Int("delay", 0, "延迟阈值(ms), 0 不过滤")
	flagSub           = flag.Bool("sub", false, "只生成订阅(CLI)")
	flagDns           = flag.Bool("dns", false, "只推送 DNS(CLI)")
	proxyFlag         = flag.String("proxy", "", "代理地址, 如 socks5://127.0.0.1:10808")
	minSpeed          = flag.Float64("minspeed", 0, "合格下载速度(MB/s)")
	minSpeedCount     = flag.Int("minspeedcount", 0, "合格 IP 数量, 达到后提前停止")
	rounds            = flag.Int("rounds", 3, "延迟测试轮数, 仅 speedtest=0 生效")
	flagHistoryEnable = flag.Bool("historyenable", false, "启用历史最优归档")
	flagHistory       = flag.String("history", "", "历史归档文件")
	flagHistoryCount  = flag.Int("historycount", 3, "每次归档数量")
	flagHistoryLimit  = flag.Int("historylimit", 1000, "归档最大行数, 0 不限制")
	flagDepth         = flag.Int("depth", 2, "输入源递归抓取深度")
	flagWeb           = flag.Bool("web", false, "启动 Web 服务")
	flagCLI           = flag.Bool("cli", false, "强制 CLI 跑一次")
	flagHost          = flag.String("host", "", "Web 监听地址")
	flagPort          = flag.Int("port", 0, "Web 监听端口")
	flagPrefix        = flag.String("prefix", "", "Web URL 路径前缀, 如 /iptest")
	flagConfig        = flag.String("config", "", "配置文件路径")
)

type AppConfig struct {
	File            []string  `json:"file"`
	OutFile         string    `json:"outfile"`
	Proxy           string    `json:"proxy"`
	Max             int       `json:"max"`
	SpeedTest       int       `json:"speedtest"`
	URL             string    `json:"url"`
	Delay           int       `json:"delay"`
	MinSpeed        float64   `json:"min_speed"`
	MinSpeedCount   int       `json:"min_speed_count"`
	Rounds          int       `json:"rounds"`
	HistoryEnable   bool      `json:"history_enable"`
	History         string    `json:"history"`
	HistoryCount    int       `json:"history_count"`
	HistoryLimit    int       `json:"history_limit"`
	TCPTimeout      int       `json:"tcp_timeout_ms"`
	ProbeTimeout    int       `json:"probe_timeout_ms"`
	DownloadTimeout int       `json:"download_timeout_ms"`
	SourceTimeout   int       `json:"source_timeout_ms"`
	SourceDepth     int       `json:"source_depth"`
	ProbeURL        string    `json:"probe_url"`
	SubConfig       SubConfig `json:"sub_config"`
	DNSConfig       DNSConfig `json:"dns_config"`
	WebConfig       WebConfig `json:"web_config"`
}

type WebConfig struct {
	Enabled    bool   `json:"enabled"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	PathPrefix string `json:"path_prefix"`
	AutoRun    bool   `json:"auto_run"`
	LogBuffer  int    `json:"log_buffer"`
	Username   string `json:"username"`
	Password   string `json:"password"`
}

var (
	cfgMu      sync.RWMutex
	appCfg     *AppConfig
	appCfgPath string
)

func setGlobalConfig(cfg *AppConfig) {
	cfgMu.Lock()
	appCfg = cfg
	cfgMu.Unlock()
}

func cloneConfig(cfg *AppConfig) *AppConfig {
	b, err := json.Marshal(cfg)
	if err != nil {
		return cfg
	}
	var out AppConfig
	if err := json.Unmarshal(b, &out); err != nil {
		return cfg
	}
	return &out
}

func currentConfig() *AppConfig {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	if appCfg == nil {
		d := defaultAppConfig()
		return &d
	}
	return cloneConfig(appCfg)
}

func configPath() string {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	if appCfgPath == "" {
		return "config.json"
	}
	return appCfgPath
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func defaultConfigPath() string {
	if strings.TrimSpace(*flagConfig) != "" {
		p, _ := filepath.Abs(*flagConfig)
		return p
	}
	if v := strings.TrimSpace(os.Getenv("CFIP_CONFIG")); v != "" {
		p, _ := filepath.Abs(v)
		return p
	}
	cwd, _ := os.Getwd()
	exe, _ := os.Executable()
	exeDir := filepath.Dir(exe)
	cands := []string{filepath.Join(cwd, "config.json")}
	if exeDir != "" && exeDir != "." {
		cands = append(cands, filepath.Join(exeDir, "config.json"))
	}
	for _, p := range cands {
		if fileExists(p) {
			return p
		}
	}
	if exeDir != "" && exeDir != "." && !strings.HasPrefix(exe, os.TempDir()) {
		return filepath.Join(exeDir, "config.json")
	}
	return filepath.Join(cwd, "config.json")
}

func defaultAppConfig() AppConfig {
	combinedTpl := "vless://AreYouOK@{ip}{domain}:{port}?encryption=none&security=tls&sni=your.domain.com&fp=chrome&type=xhttp&host=your.domain.com&path=/AreYouOK&mode=auto#{node_name_tpl}"
	return AppConfig{
		File:            []string{"html/bestip.csv"},
		OutFile:         "html/result.csv",
		Max:             100,
		SpeedTest:       5,
		URL:             defaultSpeedURL,
		Rounds:          3,
		HistoryEnable:   true,
		History:         "html/bestip.csv",
		HistoryCount:    3,
		HistoryLimit:    1000,
		TCPTimeout:      defaultTCPTimeout,
		ProbeTimeout:    defaultProbeTimeout,
		DownloadTimeout: defaultDownloadTimeout,
		SourceTimeout:   defaultSourceTimeout,
		SourceDepth:     2,
		ProbeURL:        defaultProbeURL,
		SubConfig: SubConfig{
			Enabled:          false,
			OutputDir:        "html/",
			Count:            15,
			NodeNameTpl:      "{index}{Country}{CountryCode}{ip}",
			B64Encode:        true,
			Subtype:          []string{"ip", "domain"},
			PreferredDomains: []string{"1.example.com", "2.example.com", "3.example.com"},
			Templates: []SubTemplateItem{
				{IPFileName: "subip.txt", DomainFileName: "subdomain.txt", Template: combinedTpl},
			},
		},
		DNSConfig: DNSConfig{
			Enabled: false, DomainTpl: "{index}.example.com", Count: 9, TTL: 300, Proxied: false,
		},
		WebConfig: WebConfig{
			Enabled: true, Host: "0.0.0.0", Port: 3000, PathPrefix: "",
			AutoRun: false, LogBuffer: 5000, Username: "admin", Password: "",
		},
	}
}

func dedupeStrings(in []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func normalizePrefix(p string) string {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	var segs []string
	for _, s := range strings.Split(p, "/") {
		s = strings.TrimSpace(s)
		if s == "" || s == "." || s == ".." {
			continue
		}
		var b strings.Builder
		for _, r := range s {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
				b.WriteRune(r)
			}
		}
		if b.Len() > 0 {
			segs = append(segs, b.String())
		}
	}
	if len(segs) == 0 {
		return ""
	}
	return "/" + strings.Join(segs, "/")
}

func normalizeConfig(cfg *AppConfig) {
	if strings.TrimSpace(cfg.OutFile) == "" {
		cfg.OutFile = "html/result.csv"
	}
	if cfg.Max <= 0 {
		cfg.Max = 100
	}
	if cfg.SpeedTest < 0 {
		cfg.SpeedTest = 0
	}
	if strings.TrimSpace(cfg.URL) == "" {
		cfg.URL = defaultSpeedURL
	}
	if cfg.Delay < 0 {
		cfg.Delay = 0
	}
	if cfg.MinSpeed < 0 {
		cfg.MinSpeed = 0
	}
	if cfg.MinSpeedCount < 0 {
		cfg.MinSpeedCount = 0
	}
	if cfg.Rounds <= 0 {
		cfg.Rounds = 3
	}
	if cfg.HistoryCount <= 0 {
		cfg.HistoryCount = 3
	}
	if cfg.HistoryLimit < 0 {
		cfg.HistoryLimit = 0
	}
	if cfg.TCPTimeout <= 0 {
		cfg.TCPTimeout = defaultTCPTimeout
	}
	if cfg.ProbeTimeout <= 0 {
		cfg.ProbeTimeout = defaultProbeTimeout
	}
	if cfg.DownloadTimeout <= 0 {
		cfg.DownloadTimeout = defaultDownloadTimeout
	}
	if cfg.SourceTimeout <= 0 {
		cfg.SourceTimeout = defaultSourceTimeout
	}
	if cfg.SourceDepth < 0 {
		cfg.SourceDepth = 0
	}
	if strings.TrimSpace(cfg.ProbeURL) == "" {
		cfg.ProbeURL = defaultProbeURL
	}
	cfg.File = dedupeStrings(cfg.File)
	if len(cfg.File) == 0 {
		cfg.File = []string{"ip.txt"}
	}
	cfg.Proxy = strings.TrimSpace(cfg.Proxy)
	cfg.History = strings.TrimSpace(cfg.History)

	if strings.TrimSpace(cfg.SubConfig.OutputDir) == "" {
		cfg.SubConfig.OutputDir = "html/"
	}
	if strings.TrimSpace(cfg.SubConfig.NodeNameTpl) == "" {
		cfg.SubConfig.NodeNameTpl = "{index}{country}{ip}"
	}
	if cfg.SubConfig.Count < 0 {
		cfg.SubConfig.Count = 0
	}
	cfg.SubConfig.PreferredDomains = dedupeStrings(cfg.SubConfig.PreferredDomains)
	cfg.SubConfig.Subtype = dedupeStrings(cfg.SubConfig.Subtype)

	if strings.TrimSpace(cfg.DNSConfig.DomainTpl) == "" {
		cfg.DNSConfig.DomainTpl = "{index}.example.com"
	}
	if cfg.DNSConfig.Count < 0 {
		cfg.DNSConfig.Count = 0
	}
	if cfg.DNSConfig.TTL <= 0 {
		cfg.DNSConfig.TTL = 300
	}

	if strings.TrimSpace(cfg.WebConfig.Host) == "" {
		cfg.WebConfig.Host = "0.0.0.0"
	}
	if cfg.WebConfig.Port <= 0 || cfg.WebConfig.Port > 65535 {
		cfg.WebConfig.Port = 3000
	}
	cfg.WebConfig.PathPrefix = normalizePrefix(cfg.WebConfig.PathPrefix)
	if cfg.WebConfig.LogBuffer <= 0 {
		cfg.WebConfig.LogBuffer = 5000
	}
	if strings.TrimSpace(cfg.WebConfig.Username) == "" {
		cfg.WebConfig.Username = "admin"
	}
}

func generateDefaultConfig(path string) error {
	cfg := defaultAppConfig()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		_ = os.MkdirAll(dir, 0755)
	}
	return os.WriteFile(path, data, 0644)
}

func readConfigFile(path string) (*AppConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg AppConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %v", err)
	}
	normalizeConfig(&cfg)
	return &cfg, nil
}

func loadOrCreateConfig() (*AppConfig, error) {
	path := defaultConfigPath()
	if !fileExists(path) {
		logWarnf("[cfg] 配置文件不存在, 自动生成: %s", path)
		if err := generateDefaultConfig(path); err != nil {
			return nil, fmt.Errorf("生成默认配置文件失败: %v", err)
		}
	}
	cfg, err := readConfigFile(path)
	if err != nil {
		return nil, err
	}
	cfgMu.Lock()
	appCfgPath = path
	cfgMu.Unlock()
	logf("[cfg] 配置文件: %s", path)
	return cfg, nil
}

func saveConfigToFile(cfg *AppConfig, path string) error {
	normalizeConfig(cfg)
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	if old, err := os.ReadFile(path); err == nil {
		_ = os.WriteFile(path+".bak", old, 0644)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func applyConfig(cfg *AppConfig, visited map[string]bool) {
	if visited["file"] {
		cfg.File = splitURLs(*flagFile)
	}
	if visited["outfile"] {
		cfg.OutFile = *outFile
	} else {
		*outFile = cfg.OutFile
	}
	if visited["max"] {
		cfg.Max = *maxThreads
	} else {
		*maxThreads = cfg.Max
	}
	if visited["speedtest"] {
		cfg.SpeedTest = *speedTest
	} else {
		*speedTest = cfg.SpeedTest
	}
	if visited["url"] {
		cfg.URL = *speedTestURL
	} else {
		*speedTestURL = cfg.URL
	}
	if visited["delay"] {
		cfg.Delay = *delay
	} else {
		*delay = cfg.Delay
	}
	if visited["minspeed"] {
		cfg.MinSpeed = *minSpeed
	} else {
		*minSpeed = cfg.MinSpeed
	}
	if visited["minspeedcount"] {
		cfg.MinSpeedCount = *minSpeedCount
	} else {
		*minSpeedCount = cfg.MinSpeedCount
	}
	if visited["rounds"] {
		cfg.Rounds = *rounds
	} else {
		*rounds = cfg.Rounds
	}
	if visited["proxy"] {
		cfg.Proxy = *proxyFlag
	} else {
		*proxyFlag = cfg.Proxy
	}
	if visited["historyenable"] {
		cfg.HistoryEnable = *flagHistoryEnable
	} else {
		*flagHistoryEnable = cfg.HistoryEnable
	}
	if visited["history"] {
		cfg.History = *flagHistory
	} else {
		*flagHistory = cfg.History
	}
	if visited["historycount"] {
		cfg.HistoryCount = *flagHistoryCount
	} else {
		*flagHistoryCount = cfg.HistoryCount
	}
	if visited["historylimit"] {
		cfg.HistoryLimit = *flagHistoryLimit
	} else {
		*flagHistoryLimit = cfg.HistoryLimit
	}
	if visited["depth"] {
		cfg.SourceDepth = *flagDepth
	}
	if visited["host"] {
		cfg.WebConfig.Host = *flagHost
	}
	if visited["port"] {
		cfg.WebConfig.Port = *flagPort
	}
	if visited["prefix"] {
		cfg.WebConfig.PathPrefix = *flagPrefix
	}
}

type Settings struct {
	Sources         []string      `json:"sources"`
	OutFile         string        `json:"outfile"`
	Proxy           string        `json:"proxy"`
	Max             int           `json:"max"`
	SpeedTest       int           `json:"speedtest"`
	SpeedURL        string        `json:"url"`
	Delay           int           `json:"delay"`
	MinSpeed        float64       `json:"min_speed"`
	MinSpeedCount   int           `json:"min_speed_count"`
	Rounds          int           `json:"rounds"`
	HistoryEnable   bool          `json:"history_enable"`
	History         string        `json:"history"`
	HistoryCount    int           `json:"history_count"`
	HistoryLimit    int           `json:"history_limit"`
	SourceDepth     int           `json:"source_depth"`
	ProbeURL        string        `json:"probe_url"`
	TCPTimeout      time.Duration `json:"tcp_timeout"`
	ProbeTimeout    time.Duration `json:"probe_timeout"`
	DownloadTimeout time.Duration `json:"download_timeout"`
	SourceTimeout   time.Duration `json:"source_timeout"`
}

func settingsFromConfig(cfg *AppConfig) Settings {
	c := cloneConfig(cfg)
	normalizeConfig(c)
	return Settings{
		Sources: c.File, OutFile: c.OutFile, Proxy: c.Proxy,
		Max: c.Max, SpeedTest: c.SpeedTest, SpeedURL: c.URL,
		Delay: c.Delay, MinSpeed: c.MinSpeed, MinSpeedCount: c.MinSpeedCount, Rounds: c.Rounds,
		HistoryEnable: c.HistoryEnable, History: c.History, HistoryCount: c.HistoryCount, HistoryLimit: c.HistoryLimit,
		SourceDepth: c.SourceDepth, ProbeURL: c.ProbeURL,
		TCPTimeout:      time.Duration(c.TCPTimeout) * time.Millisecond,
		ProbeTimeout:    time.Duration(c.ProbeTimeout) * time.Millisecond,
		DownloadTimeout: time.Duration(c.DownloadTimeout) * time.Millisecond,
		SourceTimeout:   time.Duration(c.SourceTimeout) * time.Millisecond,
	}
}

func urlHost(raw string) string {
	if u, err := url.Parse(strings.TrimSpace(raw)); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	if u, err := url.Parse(defaultProbeURL); err == nil {
		return u.Hostname()
	}
	return ""
}

var (
	proxyInitMu    sync.Mutex
	proxyMu        sync.RWMutex
	proxyRaw       string
	proxyActive    bool
	proxyTransport *http.Transport
)

func initProxy(raw string) bool {
	proxyInitMu.Lock()
	defer proxyInitMu.Unlock()

	proxyMu.Lock()
	old := proxyTransport
	proxyActive, proxyRaw, proxyTransport = false, raw, nil
	proxyMu.Unlock()
	if old != nil {
		old.CloseIdleConnections()
	}

	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		logErrf("[proxy] 解析代理地址失败: %v", err)
		return false
	}
	testTr := &http.Transport{Proxy: http.ProxyURL(u)}
	client := &http.Client{Transport: testTr, Timeout: 10 * time.Second}
	req, _ := http.NewRequest("GET", defaultProbeURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		testTr.CloseIdleConnections()
		logWarnf("[proxy] 代理连接失败, 将直连: %v", err)
		return false
	}
	code := resp.StatusCode
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	testTr.CloseIdleConnections()
	if code != http.StatusOK {
		logWarnf("[proxy] 代理请求 probe 返回 %d, 将直连", code)
		return false
	}
	tr := &http.Transport{Proxy: http.ProxyURL(u)}
	proxyMu.Lock()
	proxyRaw, proxyActive, proxyTransport = raw, true, tr
	proxyMu.Unlock()
	logOkf("[proxy] 代理可用: %s (%dms); 远程下载与 DNS 推送走代理", raw, time.Since(start).Milliseconds())
	return true
}

func newOutboundHTTPClient(t time.Duration) *http.Client {
	proxyMu.RLock()
	active, tr := proxyActive, proxyTransport
	proxyMu.RUnlock()
	if active && tr != nil {
		return &http.Client{Transport: tr, Timeout: t}
	}
	return &http.Client{Timeout: t}
}

func proxyStatus() (string, bool) {
	proxyMu.RLock()
	defer proxyMu.RUnlock()
	return proxyRaw, proxyActive
}

type result struct {
	ip          string
	port        int
	country     string
	countryCode string
	dataCenter  string
	outboundIP  string
	asnOrg      string
	latency     string
	tcpDuration time.Duration
}

type speedtestresult struct {
	result
	downloadSpeed float64
}

type probeResponse struct {
	Probe          string `json:"probe"`
	IP             string `json:"ip"`
	IPType         string `json:"ipType"`
	Colo           string `json:"colo"`
	ASN            uint   `json:"asn"`
	ASOrganization string `json:"asOrganization"`
	Country        string `json:"country"`
	City           string `json:"city"`
	Loc            string `json:"loc"`
	Error          string `json:"error"`
}

type RunSummary struct {
	Valid       int
	SpeedTested int
	Qualified   int
	Rows        int
	BestSpeed   float64
	BestLatency int64
	Results     []speedtestresult
}

func main() {
	flag.Parse()
	visited := make(map[string]bool)
	flag.Visit(func(f *flag.Flag) { visited[f.Name] = true })

	cfg, err := loadOrCreateConfig()
	if err != nil {
		fmt.Printf("加载配置失败: %v\n", err)
		os.Exit(1)
	}
	applyConfig(cfg, visited)
	normalizeConfig(cfg)
	setGlobalConfig(cfg)
	hub.setMax(cfg.WebConfig.LogBuffer)

	webMode := cfg.WebConfig.Enabled
	if visited["web"] {
		webMode = *flagWeb
	}
	if *flagCLI || *flagSub || *flagDns {
		webMode = false
	}
	if webMode {
		if err := runWebServer(cfg); err != nil {
			logErrf("[web] 服务退出: %v", err)
			os.Exit(1)
		}
		return
	}

	mode := "full"
	if *flagSub {
		mode = "sub"
	} else if *flagDns {
		mode = "dns"
	}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		logWarnf("收到中断信号, 正在停止任务...")
		stopJob()
	}()
	if _, err := startJob(mode, cfg); err != nil {
		logErrf("%v", err)
		os.Exit(1)
	}
	waitJob()
	jobMu.Lock()
	phase, msg := js.Phase, js.Error
	jobMu.Unlock()
	if phase == "error" {
		logErrf("任务失败: %s", msg)
		os.Exit(1)
	}
	if phase == "stopped" {
		os.Exit(130)
	}
}

var rlimitOnce sync.Once

func raiseFileLimit() {
	rlimitOnce.Do(func() {
		if runtime.GOOS == "windows" {
			return
		}
		out, err := exec.Command("sh", "-c", "ulimit -n").Output()
		if err != nil {
			return
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(out)))
		if err != nil || n <= 0 {
			return
		}
		if n < 4096 {
			logWarnf("[system] 文件描述符软上限 %d 偏低, 大并发可能受限; 建议启动前执行 ulimit -n 10000", n)
		} else {
			logf("[system] 文件描述符软上限: %d", n)
		}
	})
}

func runSpeedtest(ctx context.Context, st Settings, ips []string) (*RunSummary, error) {
	sum := &RunSummary{}
	startTime := time.Now()
	raiseFileLimit()

	total := len(ips)
	setPhase("connect", "连通性检测", total)
	logf("[connect] 开始连通性检测: %d 个 IP, 并发 %d, TCP 超时 %dms", total, st.Max, st.TCPTimeout.Milliseconds())

	var wg sync.WaitGroup
	connCh := make(chan result, total)
	sem := make(chan struct{}, st.Max)
	var done, valid int32

dispatch:
	for _, ip := range ips {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break dispatch
		}
		wg.Add(1)
		go func(ip string) {
			defer func() {
				<-sem
				wg.Done()
				c := atomic.AddInt32(&done, 1)
				setProgress(int(c), total)
				if c == int32(total) || c%20 == 0 {
					logf("[连通性检测] %d/%d (%.1f%%) 有效 %d", c, total,
						float64(c)/float64(total)*100, atomic.LoadInt32(&valid))
				}
			}()
			if ctx.Err() != nil {
				return
			}
			parts := strings.Fields(ip)
			if len(parts) != 2 {
				return
			}
			ipAddr := parts[0]
			port, err := strconv.Atoi(parts[1])
			if err != nil || port <= 0 || port > 65535 {
				return
			}
			dialer := &net.Dialer{Timeout: st.TCPTimeout}
			start := time.Now()
			conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(ipAddr, strconv.Itoa(port)))
			if err != nil {
				return
			}
			conn.Close()
			dur := time.Since(start)
			if st.Delay > 0 && dur.Milliseconds() > int64(st.Delay) {
				return
			}
			pb, perr := probeIP(ctx, st, ipAddr, port)
			if perr != nil {
				logDebugf("[probe] %s:%d 真连接失败, 已排除: %v", ipAddr, port, perr)
				return
			}
			atomic.AddInt32(&valid, 1)
			connCh <- result{
				ip: ipAddr, port: port,
				country: countryNameZh(pb.Country), countryCode: pb.Country,
				dataCenter: pb.Colo, outboundIP: pb.IP, asnOrg: pb.ASOrganization,
				latency: strconv.FormatInt(dur.Milliseconds(), 10), tcpDuration: dur,
			}
		}(ip)
	}
	wg.Wait()
	close(connCh)

	var connResults []result
	for r := range connCh {
		connResults = append(connResults, r)
	}
	sum.Valid = len(connResults)
	logOkf("[connect] 完成: 有效 IP %d / %d", len(connResults), total)

	if len(connResults) == 0 {
		logWarnf("没有发现有效的 IP, 输出空结果文件")
		_ = writeResultsCSV(st.OutFile, nil, st.SpeedTest > 0)
		return sum, ctx.Err()
	}

	if st.SpeedTest == 0 && ctx.Err() == nil {
		n := len(connResults)
		setPhase("latency", "延迟测试", n)
		logf("[latency] 开始延迟测试: %d 个 IP, 每个 %d 轮", n, st.Rounds)
		var wg3 sync.WaitGroup
		var fin int32
	loopLatency:
		for i := range connResults {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				break loopLatency
			}
			wg3.Add(1)
			go func(idx int) {
				defer func() {
					<-sem
					wg3.Done()
					c := atomic.AddInt32(&fin, 1)
					setProgress(int(c), n)
					if c == int32(n) || c%20 == 0 {
						logf("[延迟测试] %d/%d (%.1f%%)", c, n, float64(c)/float64(n)*100)
					}
				}()
				var totalDur time.Duration
				cnt := 0
				for r := 0; r < st.Rounds; r++ {
					if ctx.Err() != nil {
						return
					}
					dialer := &net.Dialer{Timeout: st.TCPTimeout}
					start := time.Now()
					conn, err := dialer.DialContext(ctx, "tcp",
						net.JoinHostPort(connResults[idx].ip, strconv.Itoa(connResults[idx].port)))
					if err == nil {
						conn.Close()
						totalDur += time.Since(start)
						cnt++
					}
				}
				if cnt > 0 {
					avg := totalDur / time.Duration(cnt)
					connResults[idx].tcpDuration = avg
					connResults[idx].latency = strconv.FormatInt(avg.Milliseconds(), 10)
				}
			}(i)
		}
		wg3.Wait()
	}

	sort.Slice(connResults, func(i, j int) bool { return connResults[i].tcpDuration < connResults[j].tcpDuration })

	var results []speedtestresult
	if st.SpeedTest > 0 && ctx.Err() == nil {
		n := len(connResults)
		setPhase("speed", "下载测速", n)
		logf("[speed] 开始下载测速: %d 个 IP, 协程 %d, 单 IP 超时 %dms", n, st.SpeedTest, st.DownloadTimeout.Milliseconds())
		speedChan := make(chan result, n)
		for _, r := range connResults {
			speedChan <- r
		}
		close(speedChan)

		var wg2 sync.WaitGroup
		var mu sync.Mutex
		var doneCount, qualified, stopFlag int32
		earlyStop := st.MinSpeed > 0 && st.MinSpeedCount > 0

		for i := 0; i < st.SpeedTest; i++ {
			wg2.Add(1)
			go func() {
				defer wg2.Done()
				for {
					select {
					case <-ctx.Done():
						return
					case res, ok := <-speedChan:
						if !ok {
							return
						}
						if earlyStop && atomic.LoadInt32(&stopFlag) == 1 {
							continue
						}
						sp := getDownloadSpeed(ctx, st, res.ip, res.port)
						mu.Lock()
						results = append(results, speedtestresult{result: res, downloadSpeed: sp})
						mu.Unlock()
						logf("[speed] %s:%d %.2f MB/s (延迟 %sms %s)", res.ip, res.port, sp, res.latency, res.country)
						if earlyStop && sp >= st.MinSpeed {
							if atomic.AddInt32(&qualified, 1) >= int32(st.MinSpeedCount) {
								if atomic.CompareAndSwapInt32(&stopFlag, 0, 1) {
									logOkf("[speed] 已达到合格 IP 数量 %d (>=%.2f MB/s), 停止后续测速", st.MinSpeedCount, st.MinSpeed)
								}
							}
						}
						c := atomic.AddInt32(&doneCount, 1)
						setProgress(int(c), n)
						if c == int32(n) || c%10 == 0 {
							logf("[测速进度] %d/%d (%.1f%%)", c, n, float64(c)/float64(n)*100)
						}
					}
				}
			}()
		}
		wg2.Wait()
		sum.SpeedTested = len(results)
		sum.Qualified = int(atomic.LoadInt32(&qualified))
		sort.Slice(results, func(i, j int) bool { return results[i].downloadSpeed > results[j].downloadSpeed })
	} else {
		for _, r := range connResults {
			results = append(results, speedtestresult{result: r})
		}
	}

	setPhase("write", "写入结果", 0)
	if err := writeResultsCSV(st.OutFile, results, st.SpeedTest > 0); err != nil {
		return sum, err
	}
	sum.Rows = len(results)
	sum.Results = results
	for _, r := range results {
		if r.downloadSpeed > sum.BestSpeed {
			sum.BestSpeed = r.downloadSpeed
		}
		ms := r.tcpDuration.Milliseconds()
		if sum.BestLatency == 0 || ms < sum.BestLatency {
			sum.BestLatency = ms
		}
	}
	logOkf("[write] 结果已写入 %s (%d 行)", st.OutFile, len(results))

	if st.SpeedTest > 0 && len(results) > 0 && st.HistoryEnable && st.History != "" {
		setPhase("history", "历史归档", 0)
		if err := appendHistory(results, st, time.Now()); err != nil {
			logErrf("[history] 归档失败: %v", err)
		}
	}
	logOkf("有效IP数量: %d | 结果文件: %s | 耗时 %.1fs", sum.Valid, st.OutFile, time.Since(startTime).Seconds())
	return sum, ctx.Err()
}

var csvHeader = []string{"IP地址", "端口号", "网络延迟ms", "下载速度", "国家", "国家代码", "数据中心", "出站IP", "ASN组织"}

func writeResultsCSV(path string, results []speedtestresult, withSpeed bool) error {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("创建输出目录失败 [%s]: %v", dir, err)
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("无法创建文件: %v", err)
	}
	defer f.Close()
	w := csv.NewWriter(f)
	_ = w.Write(csvHeader)
	for _, res := range results {
		speedStr := ""
		if withSpeed {
			speedStr = fmt.Sprintf("%.2f MB/s", res.downloadSpeed)
		}
		_ = w.Write([]string{
			res.result.ip, strconv.Itoa(res.result.port), res.result.latency, speedStr,
			res.result.country, res.result.countryCode, res.result.dataCenter,
			res.result.outboundIP, res.result.asnOrg,
		})
	}
	w.Flush()
	return w.Error()
}

func appendHistory(results []speedtestresult, st Settings, ts time.Time) error {
	count := st.HistoryCount
	if count <= 0 {
		count = 3
	}
	if count > len(results) {
		count = len(results)
	}
	if dir := filepath.Dir(st.History); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("创建历史文件目录失败 [%s]: %v", dir, err)
		}
	}
	f, err := os.OpenFile(st.History, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("打开历史文件失败 [%s]: %v", st.History, err)
	}
	if _, err := fmt.Fprintf(f, "# %s\n", ts.Format("2006-01-02 15:04:05")); err != nil {
		f.Close()
		return err
	}
	cw := csv.NewWriter(f)
	for i := 0; i < count; i++ {
		res := results[i]
		_ = cw.Write([]string{
			res.result.ip, strconv.Itoa(res.result.port),
			strconv.FormatInt(res.result.tcpDuration.Milliseconds(), 10),
			fmt.Sprintf("%.2f MB/s", res.downloadSpeed),
			res.result.country, res.result.countryCode, res.result.dataCenter,
			res.result.outboundIP, res.result.asnOrg,
		})
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		f.Close()
		return err
	}
	f.Close()
	logOkf("[history] 已归档前 %d 个结果到 %s", count, st.History)
	if st.HistoryLimit > 0 {
		removed, err := trimHistory(st.History, st.HistoryLimit)
		if err != nil {
			logWarnf("[history] 清理历史文件失败: %v", err)
		} else if removed > 0 {
			logf("[history] 超出 %d 行上限, 清理较早数据 %d 行", st.HistoryLimit, removed)
		}
	}
	return nil
}

func trimHistory(path string, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	type segment struct {
		header string
		rows   []string
	}
	var segs []segment
	var cur segment
	total := 0
	flush := func() {
		if cur.header != "" || len(cur.rows) > 0 {
			segs = append(segs, cur)
		}
		cur = segment{}
	}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			flush()
			cur.header = line
			continue
		}
		cur.rows = append(cur.rows, line)
		total++
	}
	flush()
	if total <= limit {
		return 0, nil
	}
	need := total - limit
	removed := 0
	for i := range segs {
		if need <= 0 {
			break
		}
		n := len(segs[i].rows)
		if n == 0 {
			continue
		}
		if n <= need {
			need -= n
			removed += n
			segs[i].rows = nil
			segs[i].header = ""
		} else {
			segs[i].rows = segs[i].rows[need:]
			removed += need
			need = 0
		}
	}
	var sb strings.Builder
	for _, s := range segs {
		if len(s.rows) == 0 {
			continue
		}
		if s.header != "" {
			sb.WriteString(s.header)
			sb.WriteByte('\n')
		}
		for _, r := range s.rows {
			sb.WriteString(r)
			sb.WriteByte('\n')
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(sb.String()), 0644); err != nil {
		return removed, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return removed, err
	}
	return removed, nil
}

type HistorySegment struct {
	Time string     `json:"time"`
	Rows [][]string `json:"rows"`
}

func readHistorySegments(path string) ([]HistorySegment, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	var segs []HistorySegment
	cur := HistorySegment{Time: "未标记时间"}
	total := 0
	flush := func() {
		if len(cur.Rows) > 0 {
			segs = append(segs, cur)
		}
		cur = HistorySegment{Time: "未标记时间"}
	}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			flush()
			cur.Time = strings.TrimSpace(strings.TrimPrefix(line, "#"))
			continue
		}
		rd := csv.NewReader(strings.NewReader(line))
		rd.FieldsPerRecord = -1
		rd.LazyQuotes = true
		rec, err := rd.Read()
		if err != nil {
			continue
		}
		cur.Rows = append(cur.Rows, rec)
		total++
	}
	flush()
	return segs, total, nil
}

func probeIP(ctx context.Context, st Settings, ipAddr string, port int) (*probeResponse, error) {
	target := net.JoinHostPort(ipAddr, strconv.Itoa(port))
	sni := urlHost(st.ProbeURL)
	dialer := &net.Dialer{Timeout: st.TCPTimeout}
	tr := &http.Transport{
		DialContext: func(c context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(c, network, target)
		},
		TLSClientConfig:   &tls.Config{ServerName: sni, InsecureSkipVerify: true},
		DisableKeepAlives: true,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: st.ProbeTimeout}
	req, err := http.NewRequestWithContext(ctx, "GET", st.ProbeURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Close = true
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("probe HTTP %d", resp.StatusCode)
	}
	var data probeResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, errors.New("真连接解析响应失败")
	}
	if strings.TrimSpace(data.Error) != "" {
		return nil, fmt.Errorf("真连接返回错误: %s", data.Error)
	}
	return &data, nil
}

func getDownloadSpeed(ctx context.Context, st Settings, ip string, port int) float64 {
	u := strings.TrimSpace(st.SpeedURL)
	low := strings.ToLower(u)
	if !strings.HasPrefix(low, "http://") && !strings.HasPrefix(low, "https://") {
		u = "https://" + u
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return 0
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Referer", "https://speed.cloudflare.com/")
	req.Close = true

	dialer := &net.Dialer{Timeout: st.TCPTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
	if err != nil {
		return 0
	}
	defer conn.Close()
	tr := &http.Transport{
		DialContext:       func(c context.Context, n, a string) (net.Conn, error) { return conn, nil },
		DisableKeepAlives: true,
	}
	defer tr.CloseIdleConnections()
	start := time.Now()
	client := &http.Client{Transport: tr, Timeout: st.DownloadTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0
	}
	written, _ := io.Copy(io.Discard, resp.Body)
	dur := time.Since(start)
	if dur <= 0 || written == 0 {
		return 0
	}
	return float64(written) / dur.Seconds() / 1024 / 1024
}

func splitURLs(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func fileURLToPath(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("解析 file URL 失败: %v", err)
	}
	if !strings.EqualFold(u.Scheme, "file") {
		return "", fmt.Errorf("非 file 协议: %s", rawURL)
	}
	path := u.Path
	if runtime.GOOS == "windows" && len(path) >= 3 && path[0] == '/' && path[2] == ':' {
		path = path[1:]
	}
	if path == "" {
		return "", fmt.Errorf("file URL 缺少路径: %s", rawURL)
	}
	return path, nil
}

func isLocalSource(rawURL string) bool {
	lower := strings.ToLower(rawURL)
	if strings.HasPrefix(lower, "file://") {
		return true
	}
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return false
	}
	return true
}

func downloadContent(ctx context.Context, client *http.Client, rawURL string) (string, error) {
	lower := strings.ToLower(rawURL)
	if strings.HasPrefix(lower, "file://") {
		path, err := fileURLToPath(rawURL)
		if err != nil {
			return "", err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("读取本地文件失败 [%s]: %v", path, err)
		}
		return string(data), nil
	}
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		data, err := os.ReadFile(rawURL)
		if err != nil {
			return "", fmt.Errorf("读取本地文件失败 [%s]: %v", rawURL, err)
		}
		return string(data), nil
	}
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSourceBytes))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func tryBase64Decode(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	cleaned := strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', ' ', '\t':
			return -1
		}
		return r
	}, s)
	if len(cleaned) == 0 {
		return ""
	}
	if d, err := base64.StdEncoding.DecodeString(cleaned); err == nil {
		return string(d)
	}
	if d, err := base64.RawStdEncoding.DecodeString(cleaned); err == nil {
		return string(d)
	}
	return ""
}

func extractURLs(s string) []string {
	re := regexp.MustCompile(`(?:https?|file)://[^\s"'\\<>\r\n]+`)
	return re.FindAllString(s, -1)
}

func extractIPsLikeAwk(content string) []string {
	ipRe := regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)\.(\d+)`)
	portRe := regexp.MustCompile(`^[: ](\d+)`)
	commaPortRe := regexp.MustCompile(`^,(\d+)`)
	seen := make(map[string]bool)
	var out []string
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		s := line
		for {
			locs := ipRe.FindAllStringSubmatchIndex(s, 1)
			if len(locs) == 0 {
				break
			}
			loc := locs[0]
			ip := s[loc[0]:loc[1]]
			rest := s[loc[1]:]
			ok := true
			for g := 2; g <= 8; g += 2 {
				val, _ := strconv.Atoi(s[loc[g]:loc[g+1]])
				if val > 255 {
					ok = false
					break
				}
			}
			if ok {
				if m := portRe.FindStringSubmatch(rest); m != nil {
					if m[1] != "443" {
						ok = false
					}
				} else if m := commaPortRe.FindStringSubmatch(rest); m != nil {
					p := m[1]
					c := ""
					if len(rest) > len(m[0]) {
						c = rest[len(m[0]) : len(m[0])+1]
					}
					if !(c == "." || c == "," || p == "443") {
						ok = false
					}
				}
			}
			if ok && !seen[ip] {
				seen[ip] = true
				out = append(out, ip)
			}
			if len(rest) == 0 {
				break
			}
			s = rest
		}
	}
	return out
}

func parseIPsFromContent(content string) []string {
	var out []string
	seen := make(map[string]bool)
	add := func(ipPort string) {
		if !seen[ipPort] {
			seen[ipPort] = true
			out = append(out, ipPort)
		}
	}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) == 2 {
			if ip := net.ParseIP(parts[0]); ip != nil {
				if port, err := strconv.Atoi(parts[1]); err == nil && port > 0 && port <= 65535 {
					add(parts[0] + " " + strconv.Itoa(port))
					continue
				}
			}
		}
		for _, ip := range extractIPsLikeAwk(line) {
			add(ip + " 443")
		}
	}
	return out
}

func fetchIPsFromSources(ctx context.Context, st Settings, sources []string, maxDepth int) ([]string, error) {
	type task struct {
		source string
		depth  int
	}
	client := newOutboundHTTPClient(st.SourceTimeout)
	seenSource := make(map[string]bool)
	enqueued := make(map[string]bool)
	seenIP := make(map[string]bool)
	var ipOrder []string
	var queue []task
	for _, s := range sources {
		s = strings.TrimSpace(s)
		if s == "" || enqueued[s] {
			continue
		}
		enqueued[s] = true
		queue = append(queue, task{s, 0})
	}
	processed := 0
	for len(queue) > 0 {
		if ctx.Err() != nil {
			break
		}
		t := queue[0]
		queue = queue[1:]
		if seenSource[t.source] {
			continue
		}
		seenSource[t.source] = true
		processed++
		setProgress(processed, processed+len(queue))
		kind := "远程"
		if isLocalSource(t.source) {
			kind = "本地"
		}
		logf("[source] (%d) [%s] 深度=%d: %s", processed, kind, t.depth, t.source)
		content, err := downloadContent(ctx, client, t.source)
		if err != nil {
			logWarnf("[source] 加载失败 [%s]: %v", t.source, err)
			continue
		}
		texts := []string{content}
		if dec := tryBase64Decode(content); dec != "" && dec != content {
			texts = append(texts, dec)
		}
		sourceNew := 0
		for _, text := range texts {
			for _, ipPort := range parseIPsFromContent(text) {
				if !seenIP[ipPort] {
					seenIP[ipPort] = true
					ipOrder = append(ipOrder, ipPort)
					sourceNew++
				}
			}
		}
		logf("[source]     └─ 新增 IP: %d, 累计唯一 IP: %d", sourceNew, len(ipOrder))
		if t.depth < maxDepth {
			found := 0
			for _, text := range texts {
				for _, u := range extractURLs(text) {
					if !enqueued[u] {
						enqueued[u] = true
						queue = append(queue, task{u, t.depth + 1})
						found++
					}
				}
			}
			if found > 0 {
				logf("[source]     └─ 发现新来源: %d, 队列剩余: %d", found, len(queue))
			}
		}
	}
	updateStats(func(s *RunStats) { s.Sources = processed })
	logOkf("[source] 完成: 共处理 %d 个来源, 提取唯一 IP: %d 个", processed, len(ipOrder))
	return ipOrder, nil
}

var countryCodeToChinese = map[string]string{
	"AD": "安道尔", "AE": "阿联酋", "AF": "阿富汗", "AG": "安提瓜和巴布达",
	"AI": "安圭拉", "AL": "阿尔巴尼亚", "AM": "亚美尼亚", "AO": "安哥拉",
	"AQ": "南极洲", "AR": "阿根廷", "AS": "美属萨摩亚", "AT": "奥地利",
	"AU": "澳大利亚", "AW": "阿鲁巴", "AX": "奥兰群岛", "AZ": "阿塞拜疆",
	"BA": "波黑", "BB": "巴巴多斯", "BD": "孟加拉国", "BE": "比利时",
	"BF": "布基纳法索", "BG": "保加利亚", "BH": "巴林", "BI": "布隆迪",
	"BJ": "贝宁", "BL": "圣巴泰勒米", "BM": "百慕大", "BN": "文莱",
	"BO": "玻利维亚", "BQ": "荷兰加勒比区", "BR": "巴西", "BS": "巴哈马",
	"BT": "不丹", "BV": "布韦岛", "BW": "博茨瓦纳", "BY": "白俄罗斯",
	"BZ": "伯利兹", "CA": "加拿大", "CC": "科科斯群岛", "CD": "刚果(金)",
	"CF": "中非", "CG": "刚果(布)", "CH": "瑞士", "CI": "科特迪瓦",
	"CK": "库克群岛", "CL": "智利", "CM": "喀麦隆", "CN": "中国",
	"CO": "哥伦比亚", "CR": "哥斯达黎加", "CU": "古巴", "CV": "佛得角",
	"CW": "库拉索", "CX": "圣诞岛", "CY": "塞浦路斯", "CZ": "捷克",
	"DE": "德国", "DJ": "吉布提", "DK": "丹麦", "DM": "多米尼克",
	"DO": "多米尼加", "DZ": "阿尔及利亚", "EC": "厄瓜多尔", "EE": "爱沙尼亚",
	"EG": "埃及", "EH": "西撒哈拉", "ER": "厄立特里亚", "ES": "西班牙",
	"ET": "埃塞俄比亚", "FI": "芬兰", "FJ": "斐济", "FK": "福克兰群岛",
	"FM": "密克罗尼西亚", "FO": "法罗群岛", "FR": "法国", "GA": "加蓬",
	"GB": "英国", "GD": "格林纳达", "GE": "格鲁吉亚", "GF": "法属圭亚那",
	"GG": "根西岛", "GH": "加纳", "GI": "直布罗陀", "GL": "格陵兰",
	"GM": "冈比亚", "GN": "几内亚", "GP": "瓜德罗普", "GQ": "赤道几内亚",
	"GR": "希腊", "GS": "南乔治亚和南桑威奇群岛", "GT": "危地马拉", "GU": "关岛",
	"GW": "几内亚比绍", "GY": "圭亚那", "HK": "中国香港", "HM": "赫德岛和麦克唐纳群岛",
	"HN": "洪都拉斯", "HR": "克罗地亚", "HT": "海地", "HU": "匈牙利",
	"ID": "印度尼西亚", "IE": "爱尔兰", "IL": "以色列", "IM": "马恩岛",
	"IN": "印度", "IO": "英属印度洋领地", "IQ": "伊拉克", "IR": "伊朗",
	"IS": "冰岛", "IT": "意大利", "JE": "泽西岛", "JM": "牙买加",
	"JO": "约旦", "JP": "日本", "KE": "肯尼亚", "KG": "吉尔吉斯斯坦",
	"KH": "柬埔寨", "KI": "基里巴斯", "KM": "科摩罗", "KN": "圣基茨和尼维斯",
	"KP": "朝鲜", "KR": "韩国", "KW": "科威特", "KY": "开曼群岛",
	"KZ": "哈萨克斯坦", "LA": "老挝", "LB": "黎巴嫩", "LC": "圣卢西亚",
	"LI": "列支敦士登", "LK": "斯里兰卡", "LR": "利比里亚", "LS": "莱索托",
	"LT": "立陶宛", "LU": "卢森堡", "LV": "拉脱维亚", "LY": "利比亚",
	"MA": "摩洛哥", "MC": "摩纳哥", "MD": "摩尔多瓦", "ME": "黑山",
	"MF": "法属圣马丁", "MG": "马达加斯加", "MH": "马绍尔群岛", "MK": "北马其顿",
	"ML": "马里", "MM": "缅甸", "MN": "蒙古", "MO": "中国澳门",
	"MP": "北马里亚纳群岛", "MQ": "马提尼克", "MR": "毛里塔尼亚", "MS": "蒙特塞拉特",
	"MT": "马耳他", "MU": "毛里求斯", "MV": "马尔代夫", "MW": "马拉维",
	"MX": "墨西哥", "MY": "马来西亚", "MZ": "莫桑比克", "NA": "纳米比亚",
	"NC": "新喀里多尼亚", "NE": "尼日尔", "NF": "诺福克岛", "NG": "尼日利亚",
	"NI": "尼加拉瓜", "NL": "荷兰", "NO": "挪威", "NP": "尼泊尔",
	"NR": "瑙鲁", "NU": "纽埃", "NZ": "新西兰", "OM": "阿曼",
	"PA": "巴拿马", "PE": "秘鲁", "PF": "法属波利尼西亚", "PG": "巴布亚新几内亚",
	"PH": "菲律宾", "PK": "巴基斯坦", "PL": "波兰", "PM": "圣皮埃尔和密克隆",
	"PN": "皮特凯恩群岛", "PR": "波多黎各", "PS": "巴勒斯坦", "PT": "葡萄牙",
	"PW": "帕劳", "PY": "巴拉圭", "QA": "卡塔尔", "RE": "留尼汪",
	"RO": "罗马尼亚", "RS": "塞尔维亚", "RU": "俄罗斯", "RW": "卢旺达",
	"SA": "沙特阿拉伯", "SB": "所罗门群岛", "SC": "塞舌尔", "SD": "苏丹",
	"SE": "瑞典", "SG": "新加坡", "SH": "圣赫勒拿", "SI": "斯洛文尼亚",
	"SJ": "斯瓦尔巴和扬马延", "SK": "斯洛伐克", "SL": "塞拉利昂", "SM": "圣马力诺",
	"SN": "塞内加尔", "SO": "索马里", "SR": "苏里南", "SS": "南苏丹",
	"ST": "圣多美和普林西比", "SV": "萨尔瓦多", "SX": "荷属圣马丁", "SY": "叙利亚",
	"SZ": "斯威士兰", "TC": "特克斯和凯科斯群岛", "TD": "乍得", "TF": "法属南部领地",
	"TG": "多哥", "TH": "泰国", "TJ": "塔吉克斯坦", "TK": "托克劳",
	"TL": "东帝汶", "TM": "土库曼斯坦", "TN": "突尼斯", "TO": "汤加",
	"TR": "土耳其", "TT": "特立尼达和多巴哥", "TV": "图瓦卢", "TW": "中国台湾",
	"TZ": "坦桑尼亚", "UA": "乌克兰", "UG": "乌干达", "UM": "美国本土外小岛屿",
	"US": "美国", "UY": "乌拉圭", "UZ": "乌兹别克斯坦", "VA": "梵蒂冈",
	"VC": "圣文森特和格林纳丁斯", "VE": "委内瑞拉", "VG": "英属维尔京群岛", "VI": "美属维尔京群岛",
	"VN": "越南", "VU": "瓦努阿图", "WF": "瓦利斯和富图纳", "WS": "萨摩亚",
	"YE": "也门", "YT": "马约特", "ZA": "南非", "ZM": "赞比亚", "ZW": "津巴布韦",
}

func countryNameZh(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" {
		return ""
	}
	if name, ok := countryCodeToChinese[code]; ok {
		return name
	}
	return code
}