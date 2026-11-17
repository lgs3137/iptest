package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
)

type DNSConfig struct {
	Enabled   bool   `json:"enabled"`
	Token     string `json:"token"`
	ZoneID    string `json:"zone_id"`
	DomainTpl string `json:"domain_tpl"`
	Count     int    `json:"count"`
	TTL       int    `json:"ttl"`
	Proxied   bool   `json:"proxied"`
}

type CloudflareDNSRecord struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
}

type CFListResponse struct {
	Success bool `json:"success"`
	Result  []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"result"`
}

func PushToCloudflareDNS(results []SpeedResult, cfg DNSConfig) error {
	if !cfg.Enabled {
		return fmt.Errorf("DNS 推送功能未开启")
	}
	pushCount := cfg.Count
	if pushCount <= 0 || pushCount > len(results) {
		pushCount = len(results)
	}
	for i := 0; i < pushCount; i++ {
		res := results[i]
		index := i + 1
		parsedIP := net.ParseIP(res.IP)
		if parsedIP == nil {
			fmt.Printf("无效的 IP 地址: %s\n", res.IP)
			continue
		}
		recordType := "A"
		if parsedIP.To4() == nil {
			recordType = "AAAA"
		}
		fullName := strings.ReplaceAll(cfg.DomainTpl, "{index}", fmt.Sprintf("%d", index))
		if err := createOrUpdateCFRecord(fullName, res.IP, recordType, cfg); err != nil {
			fmt.Printf("推送 DNS 失败 [%s (%s) -> %s]: %v\n", fullName, recordType, res.IP, err)
		} else {
			fmt.Printf("成功推送/更新 DNS: %s (%s) -> %s\n", fullName, recordType, res.IP)
		}
	}
	return nil
}

func createOrUpdateCFRecord(name, ip, recordType string, cfg DNSConfig) error {
	zoneID := cfg.ZoneID
	ttl := cfg.TTL
	if ttl == 0 {
		ttl = 300
	}
	client := &http.Client{}

	searchURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records?name=%s&type=%s", zoneID, name, recordType)
	reqSearch, err := http.NewRequest("GET", searchURL, nil)
	if err != nil {
		return err
	}
	setCFAuthHeaders(reqSearch, cfg)
	respSearch, err := client.Do(reqSearch)
	if err != nil {
		return err
	}
	defer respSearch.Body.Close()

	var listResp CFListResponse
	if err := json.NewDecoder(respSearch.Body).Decode(&listResp); err != nil {
		return fmt.Errorf("解析 Cloudflare 查询响应失败: %v", err)
	}

	record := CloudflareDNSRecord{Type: recordType, Name: name, Content: ip, TTL: ttl, Proxied: cfg.Proxied}
	body, _ := json.Marshal(record)

	var apiURL, method string
	if listResp.Success && len(listResp.Result) > 0 {
		recordID := listResp.Result[0].ID
		apiURL = fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records/%s", zoneID, recordID)
		method = "PUT"
	} else {
		apiURL = fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records", zoneID)
		method = "POST"
	}

	req, err := http.NewRequest(method, apiURL, bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	setCFAuthHeaders(req, cfg)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("Cloudflare API 返回状态码异常: %d", resp.StatusCode)
	}
	return nil
}

func setCFAuthHeaders(req *http.Request, cfg DNSConfig) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
}

func runDnsOnly(cfg *AppConfig) {
	if !cfg.DNSConfig.Enabled {
		fmt.Println("[dns] DNS 推送未开启 (dns_config.enabled=false)")
		return
	}
	results, err := loadSpeedResultsFromFile(*outFile)
	if err != nil {
		fmt.Printf("[dns] 加载测速结果失败: %v\n", err)
		return
	}
	if err := PushToCloudflareDNS(results, cfg.DNSConfig); err != nil {
		fmt.Printf("[dns] 推送 DNS 失败: %v\n", err)
	} else {
		fmt.Println("[dns] DNS 推送流程执行完毕！")
	}
}