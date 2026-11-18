package main

import (
	"encoding/base64"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
	if fileName == "" {
		fileName = "ip.csv"
	}
	file, err := os.Open(fileName)
	if err != nil {
		return nil, fmt.Errorf("无法打开测速结果文件 %s: %v", fileName, err)
	}
	defer file.Close()

	reader := csv.NewReader(file)
	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("解析 CSV 文件失败: %v", err)
	}

	var results []SpeedResult
	for i, record := range records {
		if len(record) == 0 {
			continue
		}
		firstCol := strings.TrimSpace(record[0])
		if i == 0 && (strings.Contains(strings.ToLower(firstCol), "ip") || strings.Contains(strings.ToLower(firstCol), "address")) {
			continue
		}
		ip := firstCol
		if ip == "" {
			continue
		}
		port := "443"
		if len(record) > 1 && strings.TrimSpace(record[1]) != "" {
			port = strings.TrimSpace(record[1])
		}
		country := ""
		if len(record) > 4 {
			country = strings.TrimSpace(record[4])
		}
		countryCode := ""
		if len(record) > 5 {
			countryCode = strings.TrimSpace(record[5])
		}
		results = append(results, SpeedResult{IP: ip, Port: port, Country: country, CountryCode: countryCode})
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("测速结果为空或未能成功提取有效IP")
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
	filePath := filepath.Join(outputDir, fileName)
	return os.WriteFile(filePath, []byte(content), 0644)
}

func GenerateAllSubscriptions(cfg SubCreateConfig) error {
	if !cfg.Sub.Enabled {
		return fmt.Errorf("订阅生成功能未开启")
	}

	results, err := LoadSpeedResults(cfg)
	if err != nil {
		return err
	}

	outputDir := cfg.Sub.OutputDir
	if outputDir == "" {
		outputDir = "html/"
	}
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("创建订阅目录失败: %v", err)
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
	hasIP := false
	hasDomain := false
	for _, st := range subtypes {
		st = strings.ToLower(strings.TrimSpace(st))
		if st == "ip" {
			hasIP = true
		}
		if st == "domain" {
			hasDomain = true
		}
	}

	for _, item := range cfg.Sub.Templates {
		tplLower := strings.ToLower(item.Template)
		tplHasIP := strings.Contains(tplLower, "{ip}")
		tplHasDomain := strings.Contains(tplLower, "{domain}")

		if hasIP && tplHasIP && item.IPFileName != "" {
			var nodes []string
			targetCount := len(results)
			if cfg.Sub.Count > 0 && cfg.Sub.Count < targetCount {
				targetCount = cfg.Sub.Count
			}
			for i := 0; i < targetCount; i++ {
				index := i + 1
				res := results[i%len(results)]
				nodeName := replaceTpl(nameTpl, map[string]string{
					"ip":          res.IP,
					"port":        res.Port,
					"index":       fmt.Sprintf("%d", index),
					"country":     res.Country,
					"countrycode": res.CountryCode,
				})
				node := replaceTpl(item.Template, map[string]string{
					"ip":            res.IP,
					"domain":        "",
					"port":          res.Port,
					"node_name_tpl": nodeName,
				})
				nodes = append(nodes, node)
			}
			if err := writeSubFile(outputDir, item.IPFileName, nodes, cfg.Sub.B64Encode); err != nil {
				fmt.Printf("写入订阅文件失败 [%s]: %v\n", item.IPFileName, err)
			} else {
				fmt.Printf("订阅文件生成成功: %s (共生成 %d 个节点)\n", filepath.Join(outputDir, item.IPFileName), len(nodes))
			}
		}

		if hasDomain && tplHasDomain && item.DomainFileName != "" && len(cfg.Sub.PreferredDomains) > 0 {
			var nodes []string
			for i, domain := range cfg.Sub.PreferredDomains {
				res := results[i%len(results)]
				node := replaceTpl(item.Template, map[string]string{
					"ip":            "",
					"domain":        domain,
					"port":          res.Port,
					"node_name_tpl": domain,
				})
				nodes = append(nodes, node)
			}
			if err := writeSubFile(outputDir, item.DomainFileName, nodes, cfg.Sub.B64Encode); err != nil {
				fmt.Printf("写入订阅文件失败 [%s]: %v\n", item.DomainFileName, err)
			} else {
				fmt.Printf("订阅文件生成成功: %s (共生成 %d 个节点)\n", filepath.Join(outputDir, item.DomainFileName), len(nodes))
			}
		}
	}
	return nil
}

func runSubOnly(cfg *AppConfig) {
	subCfg := SubCreateConfig{OutFile: *outFile, Sub: cfg.SubConfig}
	if !subCfg.Sub.Enabled {
		fmt.Println("[sub] 订阅生成未开启 (sub_config.enabled=false)")
		return
	}
	if err := GenerateAllSubscriptions(subCfg); err != nil {
		fmt.Printf("[sub] 生成订阅失败: %v\n", err)
	} else {
		fmt.Println("[sub] 订阅文件生成完成！")
	}
}