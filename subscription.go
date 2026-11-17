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
	FileName         string   `json:"file_name"`
	Template         string   `json:"template"`
	Subtype          string   `json:"subtype"`
	PreferredDomains []string `json:"preferred_domains"`
}

type SubConfig struct {
	Enabled     bool              `json:"enabled"`
	OutputDir   string            `json:"output_dir"`
	Count       int               `json:"count"`
	NodeNameTpl string            `json:"node_name_tpl"`
	B64Encode   bool              `json:"b64encode"`
	Templates   []SubTemplateItem `json:"templates"`
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

// 模板变量正则, 允许字母与下划线
var tplVarRe = regexp.MustCompile(`\{([A-Za-z_]+)\}`)

// 大小写不敏感的模板替换
func replaceTpl(tpl string, vars map[string]string) string {
	return tplVarRe.ReplaceAllStringFunc(tpl, func(m string) string {
		key := strings.ToLower(m[1 : len(m)-1])
		if v, ok := vars[key]; ok {
			return v
		}
		return m // 不认识的占位符保留原样, 交由后续步骤处理
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
		outputDir = "./html"
	}
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("创建订阅目录失败: %v", err)
	}

	nameTpl := cfg.Sub.NodeNameTpl
	if nameTpl == "" {
		nameTpl = "{index}{country}{ip}"
	}

	for _, item := range cfg.Sub.Templates {
		var nodes []string
		var targetCount int

		if item.Subtype == "domain" {
			if len(item.PreferredDomains) == 0 {
				fmt.Printf("警告: 模板 [%s] 的类型为 domain，但未配置 preferred_domains\n", item.FileName)
				continue
			}
			targetCount = len(item.PreferredDomains)
		} else {
			targetCount = len(results)
			if cfg.Sub.Count > 0 && cfg.Sub.Count < targetCount {
				targetCount = cfg.Sub.Count
			}
		}

		for i := 0; i < targetCount; i++ {
			index := i + 1
			res := results[i%len(results)]

			targetIP := res.IP
			targetPort := res.Port
			targetDomain := ""
			country := res.Country
			countryCode := res.CountryCode

			nodeName := ""
			if item.Subtype == "domain" {
				targetDomain = item.PreferredDomains[i]
				nodeName = targetDomain
			} else {
				nodeName = replaceTpl(nameTpl, map[string]string{
					"ip":          targetIP,
					"port":        targetPort,
					"index":       fmt.Sprintf("%d", index),
					"country":     country,
					"countrycode": countryCode,
				})
			}

			node := replaceTpl(item.Template, map[string]string{
				"ip":     targetIP,
				"domain": targetDomain,
				"port":   targetPort,
			})
			node = replaceTpl(node, map[string]string{"node_name_tpl": nodeName})

			nodes = append(nodes, node)
		}

		content := strings.Join(nodes, "\n")
		if cfg.Sub.B64Encode {
			content = base64.StdEncoding.EncodeToString([]byte(content))
		}

		fileName := item.FileName
		if fileName == "" {
			fileName = "subscription.txt"
		}
		filePath := filepath.Join(outputDir, fileName)
		if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
			fmt.Printf("写入订阅文件失败 [%s]: %v\n", fileName, err)
			continue
		}
		fmt.Printf("订阅文件生成成功: %s (共生成 %d 个节点)\n", filePath, len(nodes))
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