package main

import (
	"encoding/base64"
	"encoding/csv"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

type SubTemplateItem struct {
	IPFileName     string `json:"ip_file_name"`
	DomainFileName string `json:"domain_file_name"`
	Template       string `json:"template"`
}

type SubConfig struct {
	Enabled          bool              `json:"enabled"`
	OutputDir        string            `json:"output_dir"`
	Count            int               `json:"count"`
	NodeNameTpl      string            `json:"node_name_tpl"`
	B64Encode        bool              `json:"b64encode"`
	Subtype          []string          `json:"subtype"`
	PreferredDomains []string          `json:"preferred_domains"`
	Templates        []SubTemplateItem `json:"templates"`
}

type SubCreateConfig struct {
	OutFile string    `json:"outfile"`
	Sub     SubConfig `json:"sub_config"`
}

type SpeedResult struct {
	IP          string
	Port        string
	Country     string
	CountryCode string
}

type SubFileResult struct {
	Kind     string   `json:"kind"`
	FileName string   `json:"file_name"`
	Path     string   `json:"path"`
	Count    int      `json:"count"`
	Encoded  bool     `json:"encoded"`
	Nodes    []string `json:"nodes"`
	Written  bool     `json:"written"`
	Error    string   `json:"error,omitempty"`
}

var tplVarRe = regexp.MustCompile(`\{([A-Za-z_]+)\}`)

func replaceTpl(tpl string, vars map[string]string) string {
	return tplVarRe.ReplaceAllStringFunc(tpl, func(m string) string {
		key := strings.ToLower(m[1 : len(m)-1])
		if v, ok := vars[key]; ok {
			return v
		}
		return m
	})
}

func loadSpeedResultsFromFile(fileName string) ([]SpeedResult, error) {
	if strings.TrimSpace(fileName) == "" {
		fileName = "html/result.csv"
	}
	f, err := os.Open(fileName)
	if err != nil {
		return nil, fmt.Errorf("无法打开测速结果文件 %s: %v", fileName, err)
	}
	defer f.Close()
	reader := csv.NewReader(f)
	reader.FieldsPerRecord = -1
	reader.Comment = '#'
	reader.LazyQuotes = true
	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("解析 CSV 文件失败: %v", err)
	}
	var results []SpeedResult
	for i, record := range records {
		if len(record) == 0 {
			continue
		}
		first := strings.TrimSpace(strings.TrimPrefix(record[0], "\ufeff"))
		if first == "" {
			continue
		}
		low := strings.ToLower(first)
		if i == 0 && (strings.Contains(low, "ip") || strings.Contains(low, "address") || strings.Contains(first, "地址")) {
			continue
		}
		if net.ParseIP(first) == nil {
			continue
		}
		port := "443"
		if len(record) > 1 {
			p := strings.TrimSpace(record[1])
			if n, err := strconv.Atoi(p); err == nil && n > 0 && n <= 65535 {
				port = strconv.Itoa(n)
			}
		}
		country, code := "", ""
		if len(record) > 4 {
			country = strings.TrimSpace(record[4])
		}
		if len(record) > 5 {
			code = strings.TrimSpace(record[5])
		}
		results = append(results, SpeedResult{IP: first, Port: port, Country: country, CountryCode: code})
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("测速结果为空或未能成功提取有效IP (%s)", fileName)
	}
	return results, nil
}

func LoadSpeedResults(cfg SubCreateConfig) ([]SpeedResult, error) {
	return loadSpeedResultsFromFile(cfg.OutFile)
}

func writeSubFile(outputDir, fileName string, nodes []string, b64 bool) error {
	content := strings.Join(nodes, "\n")
	if b64 {
		content = base64.StdEncoding.EncodeToString([]byte(content))
	}
	return os.WriteFile(filepath.Join(outputDir, fileName), []byte(content), 0644)
}

func BuildSubscriptions(cfg SubCreateConfig) ([]SubFileResult, error) {
	if !cfg.Sub.Enabled {
		return nil, fmt.Errorf("订阅生成功能未开启 (sub_config.enabled=false)")
	}
	results, err := LoadSpeedResults(cfg)
	if err != nil {
		return nil, err
	}
	outputDir := cfg.Sub.OutputDir
	if outputDir == "" {
		outputDir = "html/"
	}
	nameTpl := cfg.Sub.NodeNameTpl
	if nameTpl == "" {
		nameTpl = "{index}{country}{ip}"
	}
	subtypes := cfg.Sub.Subtype
	if len(subtypes) == 0 {
		if len(cfg.Sub.PreferredDomains) > 0 {
			subtypes = []string{"ip", "domain"}
		} else {
			subtypes = []string{"ip"}
		}
	}
	hasIP, hasDomain := false, false
	for _, st := range subtypes {
		switch strings.ToLower(strings.TrimSpace(st)) {
		case "ip":
			hasIP = true
		case "domain":
			hasDomain = true
		}
	}
	if len(cfg.Sub.Templates) == 0 {
		return nil, fmt.Errorf("未配置订阅模板 (sub_config.templates)")
	}

	var out []SubFileResult
	for _, item := range cfg.Sub.Templates {
		tplLower := strings.ToLower(item.Template)
		tplHasIP := strings.Contains(tplLower, "{ip}")
		tplHasDomain := strings.Contains(tplLower, "{domain}")

		if hasIP && tplHasIP && item.IPFileName != "" {
			targetCount := len(results)
			if cfg.Sub.Count > 0 && cfg.Sub.Count < targetCount {
				targetCount = cfg.Sub.Count
			}
			var nodes []string
			for i := 0; i < targetCount; i++ {
				res := results[i%len(results)]
				vars := map[string]string{
					"ip": res.IP, "port": res.Port,
					"index":       strconv.Itoa(i + 1),
					"country":     res.Country,
					"countrycode": res.CountryCode,
					"domain":      "",
				}
				vars["node_name_tpl"] = replaceTpl(nameTpl, vars)
				nodes = append(nodes, replaceTpl(item.Template, vars))
			}
			out = append(out, SubFileResult{
				Kind: "ip", FileName: item.IPFileName,
				Path: filepath.Join(outputDir, item.IPFileName),
				Count: len(nodes), Encoded: cfg.Sub.B64Encode, Nodes: nodes,
			})
		}

		if hasDomain && tplHasDomain && item.DomainFileName != "" && len(cfg.Sub.PreferredDomains) > 0 {
			var nodes []string
			for i, domain := range cfg.Sub.PreferredDomains {
				res := results[i%len(results)]
				nodeName := domain
				if strings.Contains(strings.ToLower(nameTpl), "{domain}") {
					nodeName = replaceTpl(nameTpl, map[string]string{
						"domain": domain, "index": strconv.Itoa(i + 1),
						"ip": res.IP, "port": res.Port,
						"country": res.Country, "countrycode": res.CountryCode,
					})
				}
				nodes = append(nodes, replaceTpl(item.Template, map[string]string{
					"ip": "", "domain": domain, "port": res.Port, "node_name_tpl": nodeName,
				}))
			}
			out = append(out, SubFileResult{
				Kind: "domain", FileName: item.DomainFileName,
				Path: filepath.Join(outputDir, item.DomainFileName),
				Count: len(nodes), Encoded: cfg.Sub.B64Encode, Nodes: nodes,
			})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("没有匹配的订阅模板: 请检查 subtype 与模板中的 {ip}/{domain} 占位符")
	}
	return out, nil
}

func GenerateAllSubscriptions(cfg SubCreateConfig) ([]SubFileResult, error) {
	files, err := BuildSubscriptions(cfg)
	if err != nil {
		return nil, err
	}
	outputDir := cfg.Sub.OutputDir
	if outputDir == "" {
		outputDir = "html/"
	}
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return files, fmt.Errorf("创建订阅目录失败: %v", err)
	}
	for i := range files {
		f := &files[i]
		if err := writeSubFile(outputDir, f.FileName, f.Nodes, cfg.Sub.B64Encode); err != nil {
			f.Error = err.Error()
			logErrf("[sub] 写入订阅文件失败 [%s]: %v", f.FileName, err)
			continue
		}
		f.Written = true
		logOkf("[sub] 订阅文件生成成功: %s (%d 个节点, base64=%v)", f.Path, f.Count, f.Encoded)
	}
	return files, nil
}