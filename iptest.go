package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
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
	timeout = 1 * time.Second

	probeURL     = "https://probe.609909.xyz/"
	probeHost    = "probe.609909.xyz"
	probeTimeout = 3 * time.Second
)

var (
	File         = flag.String("file", "ip.txt", "IP地址文件名称, 支持路径, 格式为 ip port")
	outFile      = flag.String("outfile", "ip.csv", "输出文件名称, 支持路径")
	maxThreads   = flag.Int("max", 100, "并发请求最大协程数")
	speedTest    = flag.Int("speedtest", 5, "下载测速协程数量,设为0禁用测速")
	speedTestURL = flag.String("url", "speed.cloudflare.com/__down?bytes=20000000", "测速文件地址")
	delay        = flag.Int("delay", 0, "延迟阈值(ms)，默认为0禁用延迟过滤")

	flagSub  = flag.Bool("sub", false, "只读取测速结果文件生成订阅")
	flagDns  = flag.Bool("dns", false, "只读取测速结果文件推送 DNS")
	flagUrls = flag.String("urls", "", "递归下载 URL 列表中的 IP 作为测速输入, 逗号分隔; 支持 file:// 或裸本地路径")

	proxyFlag = flag.String("proxy", "", "代理地址, 例如 socks5://127.0.0.1:10808, 为空则直连; 仅用于 urls 下载与 DNS 推送")

	minSpeed      = flag.Float64("minspeed", 0, "下载速度合格指标(MB/s), 0为不限制, 例如2表示2MB/s, 支持小数如1.5")
	minSpeedCount = flag.Int("minspeedcount", 0, "达到合格速度的IP数量限制, 达到后提前停止测速, 0为不限制")

	// 历史最优归档
	flagHistoryEnable = flag.Bool("historyenable", false, "启用历史最优归档")
	flagHistory       = flag.String("history", "", "历史最优归档文件, 例如 bestip.csv")
	flagHistoryCount  = flag.Int("historycount", 3, "每次归档的排名数量, 默认 3")
	flagHistoryLimit  = flag.Int("historylimit", 1000, "历史归档文件最大数据行数, 超出后清理较早数据; 0 表示不限制")
)

var (
	proxyURL       *url.URL
	proxyActive    bool
	proxyTransport *http.Transport
)

func initProxy(raw string) bool {
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		fmt.Printf("[proxy] 解析代理地址失败: %v\n", err)
		return false
	}
	testTr := &http.Transport{Proxy: http.ProxyURL(u)}
	client := &http.Client{Transport: testTr, Timeout: 10 * time.Second}
	req, _ := http.NewRequest("GET", probeURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := client.Do(req)
	if err != nil {
		testTr.CloseIdleConnections()
		fmt.Printf("[proxy] 代理连接失败, 将直连: %v\n", err)
		return false
	}
	statusOK := resp.StatusCode == http.StatusOK
	statusCode := resp.StatusCode
	resp.Body.Close()
	testTr.CloseIdleConnections()
	if !statusOK {
		fmt.Printf("[proxy] 代理请求 probe 返回 %d, 将直连\n", statusCode)
		return false
	}
	fmt.Printf("[proxy] 代理可用: %s ; 后续 urls 下载与 dns 推送走代理\n", raw)
	proxyURL = u
	proxyActive = true
	proxyTransport = &http.Transport{Proxy: http.ProxyURL(u)}
	return true
}

func newOutboundHTTPClient(t time.Duration) *http.Client {
	if proxyActive && proxyTransport != nil {
		return &http.Client{Transport: proxyTransport, Timeout: t}
	}
	return &http.Client{Timeout: t}
}

type AppConfig struct {
	File          string    `json:"file"`
	OutFile       string    `json:"outfile"`
	URLs          string    `json:"urls"`
	Proxy         string    `json:"proxy"`
	Max           int       `json:"max"`
	SpeedTest     int       `json:"speedtest"`
	URL           string    `json:"url"`
	Delay         int       `json:"delay"`
	MinSpeed      float64   `json:"min_speed"`
	MinSpeedCount int       `json:"min_speed_count"`
	HistoryEnable bool      `json:"history_enable"`
	History       string    `json:"history"`
	HistoryCount  int       `json:"history_count"`
	HistoryLimit  int       `json:"history_limit"`
	SubConfig     SubConfig `json:"sub_config"`
	DNSConfig     DNSConfig `json:"dns_config"`
}

func defaultConfigPath() string {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		return "config.json"
	}
	return filepath.Join(filepath.Dir(exe), "config.json")
}

func generateDefaultConfig(path string) error {
	ipTpl := "vless://AreYouOK@{ip}:{port}?encryption=none&security=tls&sni=your.domain.com&fp=chrome&type=xhttp&host=your.domain.com&path=%2FAreYouOK&mode=auto#{node_name_tpl}"
	domainTpl := "vless://AreYouOK@{domain}:{port}?encryption=none&security=tls&sni=your.domain.com&fp=chrome&type=xhttp&host=your.domain.com&path=%2FAreYouOK&mode=auto#{domain}"

	cfg := AppConfig{
		File:          "ip.txt",
		OutFile:       "ip.csv",
		URLs:          "",
		Proxy:         "",
		Max:           100,
		SpeedTest:     5,
		URL:           "speed.cloudflare.com/__down?bytes=20000000",
		Delay:         0,
		MinSpeed:      0,
		MinSpeedCount: 0,
		HistoryEnable: false,
		History:       "",
		HistoryCount:  3,
		HistoryLimit:  1000,
		SubConfig: SubConfig{
			Enabled:     false,
			OutputDir:   "./html",
			Count:       15,
			NodeNameTpl: "{index}{Country}{CountryCode}{ip}",
			B64Encode:   true,
			Templates: []SubTemplateItem{
				{
					FileName:         "subip.txt",
					Template:         ipTpl,
					Subtype:          "ip",
					PreferredDomains: []string{},
				},
				{
					FileName: "subdomain.txt",
					Template: domainTpl,
					Subtype:  "domain",
					PreferredDomains: []string{
						"1.example.com", "2.example.com", "3.example.com",
					},
				},
			},
		},
		DNSConfig: DNSConfig{
			Enabled:   false,
			Token:     "",
			ZoneID:    "",
			DomainTpl: "{index}.example.com",
			Count:     9,
			TTL:       300,
			Proxied:   false,
		},
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		_ = os.MkdirAll(dir, 0755)
	}
	return os.WriteFile(path, data, 0644)
}

func loadOrCreateConfig() (*AppConfig, error) {
	path := defaultConfigPath()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		fmt.Printf("配置文件不存在, 自动生成: %s\n", path)
		if err := generateDefaultConfig(path); err != nil {
			return nil, fmt.Errorf("生成默认配置文件失败: %v", err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg AppConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %v", err)
	}
	return &cfg, nil
}

func applyConfig(cfg *AppConfig, visited map[string]bool) {
	if !visited["file"] && cfg.File != "" {
		*File = cfg.File
	}
	if !visited["outfile"] && cfg.OutFile != "" {
		*outFile = cfg.OutFile
	}
	if !visited["max"] && cfg.Max > 0 {
		*maxThreads = cfg.Max
	}
	if !visited["speedtest"] && cfg.SpeedTest > 0 {
		*speedTest = cfg.SpeedTest
	}
	if !visited["url"] && cfg.URL != "" {
		*speedTestURL = cfg.URL
	}
	if !visited["delay"] {
		*delay = cfg.Delay
	}
	if !visited["minspeed"] && cfg.MinSpeed > 0 {
		*minSpeed = cfg.MinSpeed
	}
	if !visited["minspeedcount"] && cfg.MinSpeedCount > 0 {
		*minSpeedCount = cfg.MinSpeedCount
	}
	if !visited["proxy"] && cfg.Proxy != "" {
		*proxyFlag = cfg.Proxy
	}
	if !visited["historyenable"] {
		*flagHistoryEnable = cfg.HistoryEnable
	}
	if !visited["history"] && cfg.History != "" {
		*flagHistory = cfg.History
	}
	if !visited["historycount"] && cfg.HistoryCount > 0 {
		*flagHistoryCount = cfg.HistoryCount
	}
	if !visited["historylimit"] {
		if cfg.HistoryLimit >= 0 {
			*flagHistoryLimit = cfg.HistoryLimit
		}
	}
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
	"YE": "也门", "YT": "马约特", "ZA": "南非", "ZM": "赞比亚",
	"ZW": "津巴布韦",
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

func increaseMaxOpenFiles() {
	cmd := exec.Command("sh", "-c", "ulimit -n 10000")
	_, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Printf("提升文件描述符上限时出现错误: %v\n", err)
	}
}

func main() {
	flag.Parse()

	visited := make(map[string]bool)
	flag.Visit(func(f *flag.Flag) { visited[f.Name] = true })

	if *flagSub || *flagDns {
		cfg, err := loadOrCreateConfig()
		if err != nil {
			fmt.Printf("加载配置失败: %v\n", err)
			return
		}
		applyConfig(cfg, visited)
		if *flagSub {
			runSubOnly(cfg)
			return
		}
		if *flagDns {
			initProxy(*proxyFlag)
			runDnsOnly(cfg)
			return
		}
	}

	cfg, err := loadOrCreateConfig()
	if err != nil {
		fmt.Printf("加载配置失败: %v\n", err)
		return
	}
	applyConfig(cfg, visited)

	initProxy(*proxyFlag)

	urlsStr := *flagUrls
	if urlsStr == "" && cfg.URLs != "" {
		urlsStr = cfg.URLs
	}
	if urlsStr != "" {
		fmt.Printf("开始从 URL 提取 IP 源: %s\n", urlsStr)
		urlList := splitURLs(urlsStr)
		ips, err := fetchIPsFromURLs(urlList, 2)
		if err != nil {
			fmt.Printf("下载 IP 源失败: %v\n", err)
			return
		}
		if len(ips) == 0 {
			fmt.Println("未从 URL 中提取到有效 IP")
			return
		}
		if err := writeIPs(*File, ips); err != nil {
			fmt.Printf("写入 IP 文件失败: %v\n", err)
			return
		}
		fmt.Printf("已从 URL 提取 %d 个 IP 写入 %s\n", len(ips), *File)
	}

	runSpeedtest(cfg)

	if cfg.SubConfig.Enabled {
		fmt.Println("[sub] 测速完成, 开始生成订阅...")
		runSubOnly(cfg)
	}
	if cfg.DNSConfig.Enabled {
		fmt.Println("[dns] 测速完成, 开始推送 DNS...")
		runDnsOnly(cfg)
	}
}

func runSpeedtest(cfg *AppConfig) {
	var validCount int32
	startTime := time.Now()
	osType := runtime.GOOS
	if osType == "linux" {
		increaseMaxOpenFiles()
	}

	ips, err := readIPs(*File)
	if err != nil {
		fmt.Printf("无法从文件中读取 IP: %v\n", err)
		return
	}

	var wg sync.WaitGroup
	wg.Add(len(ips))

	resultChan := make(chan result, len(ips))
	thread := make(chan struct{}, *maxThreads)

	var count int
	total := len(ips)

	for _, ip := range ips {
		thread <- struct{}{}
		go func(ip string) {
			defer func() {
				<-thread
				wg.Done()
				count++
				percentage := float64(count) / float64(total) * 100
				fmt.Printf("已完成: %d 总数: %d 已完成: %.2f%%\r", count, total, percentage)
				if count == total {
					fmt.Printf("已完成: %d 总数: %d 已完成: %.2f%%\n", count, total, percentage)
				}
			}()

			parts := strings.Fields(ip)
			if len(parts) != 2 {
				return
			}
			ipAddr := parts[0]
			port, err := strconv.Atoi(parts[1])
			if err != nil {
				return
			}

			dialer := &net.Dialer{Timeout: timeout, KeepAlive: 0}
			start := time.Now()
			conn, err := dialer.Dial("tcp", net.JoinHostPort(ipAddr, strconv.Itoa(port)))
			if err != nil {
				return
			}
			conn.Close()

			tcpDuration := time.Since(start)
			if *delay > 0 && tcpDuration.Milliseconds() > int64(*delay) {
				return
			}

			probe, perr := probeIP(ipAddr, port)
			if perr != nil {
				fmt.Printf("IP %s 端口 %d 真连接请求失败, 已排除\n", ipAddr, port)
				return
			}

			atomic.AddInt32(&validCount, 1)

			res := result{
				ip:          ipAddr,
				port:        port,
				country:     countryNameZh(probe.Country),
				countryCode: probe.Country,
				dataCenter:  probe.Colo,
				outboundIP:  probe.IP,
				asnOrg:      probe.ASOrganization,
				latency:     fmt.Sprintf("%d", tcpDuration.Milliseconds()),
				tcpDuration: tcpDuration,
			}

			resultChan <- res
		}(ip)
	}

	wg.Wait()
	close(resultChan)

	if len(resultChan) == 0 {
		fmt.Println("没有发现有效的IP")
		return
	}

	var results []speedtestresult
	if *speedTest > 0 {
		fmt.Printf("找到符合条件的IP 共 %d 个\n", atomic.LoadInt32(&validCount))
		fmt.Printf("开始测速\n")

		total := len(resultChan)
		var wg2 sync.WaitGroup
		wg2.Add(*speedTest)

		var mu sync.Mutex
		var doneCount int32
		var qualified int32
		var stop int32

		minSp := *minSpeed
		minCnt := *minSpeedCount
		enableEarlyStop := minSp > 0 && minCnt > 0

		for i := 0; i < *speedTest; i++ {
			thread <- struct{}{}
			go func() {
				defer func() {
					<-thread
					wg2.Done()
				}()
				for res := range resultChan {
					if enableEarlyStop && atomic.LoadInt32(&stop) == 1 {
						continue
					}
					downloadSpeed := getDownloadSpeed(res.ip, res.port)

					mu.Lock()
					results = append(results, speedtestresult{result: res, downloadSpeed: downloadSpeed})
					mu.Unlock()

					if enableEarlyStop && downloadSpeed >= minSp {
						if atomic.AddInt32(&qualified, 1) >= int32(minCnt) {
							if atomic.CompareAndSwapInt32(&stop, 0, 1) {
								fmt.Printf("\n已达到合格 IP 数量 %d (速度>=%.2f MB/s), 停止后续测速\n", minCnt, minSp)
							}
						}
					}

					c := atomic.AddInt32(&doneCount, 1)
					percentage := float64(c) / float64(total) * 100
					fmt.Printf("已完成: %.2f%%\r", percentage)
					if int(c) >= total {
						fmt.Printf("已完成: %.2f%%\033[0\n", percentage)
					}
				}
			}()
		}
		wg2.Wait()

		if enableEarlyStop && atomic.LoadInt32(&stop) == 1 {
			fmt.Println()
		}
	} else {
		for res := range resultChan {
			results = append(results, speedtestresult{result: res})
		}
	}

	if *speedTest > 0 {
		sort.Slice(results, func(i, j int) bool {
			return results[i].downloadSpeed > results[j].downloadSpeed
		})
	} else {
		sort.Slice(results, func(i, j int) bool {
			return results[i].result.tcpDuration < results[j].result.tcpDuration
		})
	}

	if dir := filepath.Dir(*outFile); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			fmt.Printf("创建输出目录失败 [%s]: %v\n", dir, err)
			return
		}
	}

	file, err := os.Create(*outFile)
	if err != nil {
		fmt.Printf("无法创建文件: %v\n", err)
		return
	}
	defer file.Close()
	writer := csv.NewWriter(file)

	writer.Write([]string{
		"IP地址", "端口号", "网络延迟ms", "下载速度",
		"国家", "国家代码", "数据中心", "出站IP", "ASN组织",
	})

	for _, res := range results {
		speedStr := ""
		if *speedTest > 0 {
			speedStr = fmt.Sprintf("%.2f MB/s", res.downloadSpeed)
		}
		writer.Write([]string{
			res.result.ip,
			strconv.Itoa(res.result.port),
			res.result.latency,
			speedStr,
			res.result.country,
			res.result.countryCode,
			res.result.dataCenter,
			res.result.outboundIP,
			res.result.asnOrg,
		})
	}
	writer.Flush()

	if *speedTest > 0 && len(results) > 0 && cfg.HistoryEnable && cfg.History != "" {
		if err := appendHistory(results, cfg, time.Now()); err != nil {
			fmt.Printf("[history] 归档失败: %v\n", err)
		}
	}

	fmt.Printf("有效IP数量: %d | 成功将结果写入文件 %s，耗时 %d秒\n",
		atomic.LoadInt32(&validCount), *outFile, time.Since(startTime)/time.Second)
}

func appendHistory(results []speedtestresult, cfg *AppConfig, ts time.Time) error {
	count := cfg.HistoryCount
	if count <= 0 {
		count = 3
	}
	if count > len(results) {
		count = len(results)
	}

	if dir := filepath.Dir(cfg.History); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("创建历史文件目录失败 [%s]: %v", dir, err)
		}
	}

	f, err := os.OpenFile(cfg.History, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("打开历史文件失败 [%s]: %v", cfg.History, err)
	}

	if _, err := fmt.Fprintf(f, "# %s\n", ts.Format("2006-01-02 15:04:05")); err != nil {
		f.Close()
		return err
	}

	cw := csv.NewWriter(f)
	for i := 0; i < count; i++ {
		res := results[i]
		cw.Write([]string{
			res.result.ip,
			strconv.Itoa(res.result.port),
			strconv.FormatInt(res.result.tcpDuration.Milliseconds(), 10),
			fmt.Sprintf("%.2f MB/s", res.downloadSpeed),
			res.result.country,
			res.result.countryCode,
			res.result.dataCenter,
			res.result.outboundIP,
			res.result.asnOrg,
		})
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		f.Close()
		return err
	}
	f.Close()

	fmt.Printf("[history] 已归档前 %d 个结果到 %s\n", count, cfg.History)

	if cfg.HistoryLimit > 0 {
		removed, err := trimHistory(cfg.History, cfg.HistoryLimit)
		if err != nil {
			fmt.Printf("[history] 清理历史文件失败: %v\n", err)
		} else if removed > 0 {
			fmt.Printf("[history] 历史文件已超出 %d 行上限, 清理较早数据 %d 行\n", cfg.HistoryLimit, removed)
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

func probeIP(ipAddr string, port int) (*probeResponse, error) {
	targetAddr := net.JoinHostPort(ipAddr, strconv.Itoa(port))
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 0}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, targetAddr)
		},
		TLSClientConfig: &tls.Config{ServerName: probeHost, InsecureSkipVerify: true},
	}
	defer tr.CloseIdleConnections()
	client := http.Client{Transport: tr, Timeout: probeTimeout}
	req, _ := http.NewRequest("GET", probeURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Close = true
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var data probeResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("真连接解析响应失败")
	}
	if data.Error != "" {
		return nil, fmt.Errorf("真连接返回错误")
	}
	return &data, nil
}

func readIPs(file string) ([]string, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var ips []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Fields(line)
		if len(parts) != 2 {
			continue
		}
		port, err := strconv.Atoi(parts[1])
		if err != nil {
			continue
		}
		ips = append(ips, fmt.Sprintf("%s %d", parts[0], port))
	}
	return ips, scanner.Err()
}

func getDownloadSpeed(ip string, port int) float64 {
	protocol := "https://"
	req, _ := http.NewRequest("GET", protocol+*speedTestURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Referer", "https://speed.cloudflare.com/")

	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 0}
	conn, err := dialer.Dial("tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
	if err != nil {
		return 0
	}
	defer conn.Close()

	startTime := time.Now()
	client := http.Client{
		Transport: &http.Transport{
			Dial: func(network, addr string) (net.Conn, error) { return conn, nil },
		},
		Timeout: 5 * time.Second,
	}
	req.Close = true
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	written, _ := io.Copy(io.Discard, resp.Body)
	duration := time.Since(startTime)
	speed := float64(written) / duration.Seconds() / 1024 / 1024
	fmt.Printf("IP %s 端口 %d 下载速度 %.2f MB/s\n", ip, port, speed)
	return speed
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

func downloadContent(client *http.Client, rawURL string) (string, error) {
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

	req, err := http.NewRequest("GET", rawURL, nil)
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
	body, err := io.ReadAll(resp.Body)
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
	if decoded, err := base64.StdEncoding.DecodeString(cleaned); err == nil {
		return string(decoded)
	}
	if decoded, err := base64.RawStdEncoding.DecodeString(cleaned); err == nil {
		return string(decoded)
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

func fetchIPsFromURLs(urls []string, maxDepth int) ([]string, error) {
	type task struct {
		url   string
		depth int
	}
	client := newOutboundHTTPClient(20 * time.Second)

	seenURL := make(map[string]bool)
	enqueued := make(map[string]bool)
	seenIP := make(map[string]bool)
	var ipOrder []string

	var queue []task
	for _, u := range urls {
		if !enqueued[u] {
			enqueued[u] = true
			queue = append(queue, task{u, 0})
		}
	}
	fmt.Printf("[urls] 初始待处理 URL: %d 个\n", len(queue))

	var downloaded int
	for len(queue) > 0 {
		t := queue[0]
		queue = queue[1:]
		if seenURL[t.url] {
			continue
		}
		seenURL[t.url] = true
		downloaded++

		kind := "请求远程"
		if isLocalSource(t.url) {
			kind = "读取本地"
		}
		fmt.Printf("[urls] (%d) %s 深度=%d: %s\n", downloaded, kind, t.depth, t.url)

		content, err := downloadContent(client, t.url)
		if err != nil {
			fmt.Printf("[urls] 加载失败 [%s]: %v\n", t.url, err)
			continue
		}

		texts := []string{content}
		if dec := tryBase64Decode(content); dec != "" && dec != content {
			texts = append(texts, dec)
		}

		urlNew := 0
		for _, text := range texts {
			for _, ip := range extractIPsLikeAwk(text) {
				if !seenIP[ip] {
					seenIP[ip] = true
					ipOrder = append(ipOrder, ip)
					urlNew++
				}
			}
		}
		fmt.Printf("[urls]     └─ 新增 IP: %d, 累计唯一 IP: %d\n", urlNew, len(ipOrder))

		if t.depth < maxDepth {
			urlFound := 0
			for _, text := range texts {
				for _, u := range extractURLs(text) {
					if !enqueued[u] {
						enqueued[u] = true
						queue = append(queue, task{u, t.depth + 1})
						urlFound++
					}
				}
			}
			if urlFound > 0 {
				fmt.Printf("[urls]     └─ 发现新 URL: %d, 队列剩余: %d\n", urlFound, len(queue))
			}
		}
	}

	fmt.Printf("[urls] 递归完成: 共处理 %d 个来源, 提取唯一 IP 共 %d 个\n", downloaded, len(ipOrder))
	return ipOrder, nil
}

func writeIPs(fileName string, ips []string) error {
	if dir := filepath.Dir(fileName); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("创建目录失败 [%s]: %v", dir, err)
		}
	}
	f, err := os.Create(fileName)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, ip := range ips {
		fmt.Fprintf(w, "%s 443\n", ip)
	}
	return w.Flush()
}