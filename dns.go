package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
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

type CFError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type CFRecord struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
}

type CFListResponse struct {
	Success bool       `json:"success"`
	Errors  []CFError  `json:"errors"`
	Result  []CFRecord `json:"result"`
}

type DNSPlanItem struct {
	Index   int    `json:"index"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
	Country string `json:"country"`
}

func cfErrText(errs []CFError) string {
	var parts []string
	for _, e := range errs {
		parts = append(parts, fmt.Sprintf("%d: %s", e.Code, e.Message))
	}
	if len(parts) == 0 {
		return "未知错误"
	}
	return strings.Join(parts, "; ")
}

func checkDNSCfg(cfg DNSConfig) error {
	if strings.TrimSpace(cfg.Token) == "" || strings.TrimSpace(cfg.ZoneID) == "" {
		return errors.New("未配置 dns_config.token 或 dns_config.zone_id")
	}
	return nil
}

func PlanDNSRecords(results []SpeedResult, cfg DNSConfig) []DNSPlanItem {
	n := cfg.Count
	if n <= 0 || n > len(results) {
		n = len(results)
	}
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = 300
	}
	var items []DNSPlanItem
	for i := 0; i < n; i++ {
		res := results[i]
		parsed := net.ParseIP(res.IP)
		if parsed == nil {
			continue
		}
		rt := "A"
		if parsed.To4() == nil {
			rt = "AAAA"
		}
		name := replaceTpl(cfg.DomainTpl, map[string]string{
			"index":       strconv.Itoa(i + 1),
			"ip":          res.IP,
			"port":        res.Port,
			"country":     res.Country,
			"countrycode": res.CountryCode,
		})
		items = append(items, DNSPlanItem{
			Index: i + 1, Name: name, Type: rt, Content: res.IP,
			TTL: ttl, Proxied: cfg.Proxied, Country: res.Country,
		})
	}
	return items
}

func PushToCloudflareDNS(ctx context.Context, results []SpeedResult, cfg DNSConfig) (int, int, error) {
	if !cfg.Enabled {
		return 0, 0, errors.New("DNS 推送功能未开启 (dns_config.enabled=false)")
	}
	if err := checkDNSCfg(cfg); err != nil {
		return 0, 0, err
	}
	items := PlanDNSRecords(results, cfg)
	setPhase("dns", "推送 DNS", len(items))
	pushed, failed := 0, 0
	for i, it := range items {
		if ctx.Err() != nil {
			return pushed, failed, ctx.Err()
		}
		if err := createOrUpdateCFRecord(ctx, it.Name, it.Content, it.Type, cfg); err != nil {
			failed++
			logErrf("[dns] 推送失败 [%s (%s) -> %s]: %v", it.Name, it.Type, it.Content, err)
		} else {
			pushed++
			logOkf("[dns] 成功推送/更新: %s (%s) -> %s", it.Name, it.Type, it.Content)
		}
		setProgress(i+1, len(items))
	}
	logf("[dns] 推送完成: 成功 %d, 失败 %d", pushed, failed)
	return pushed, failed, nil
}

func createOrUpdateCFRecord(ctx context.Context, name, ip, recordType string, cfg DNSConfig) error {
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = 300
	}
	client := newOutboundHTTPClient(30 * time.Second)
	searchURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records?name=%s&type=%s",
		cfg.ZoneID, url.QueryEscape(name), recordType)
	reqSearch, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return err
	}
	setCFAuthHeaders(reqSearch, cfg)
	respSearch, err := client.Do(reqSearch)
	if err != nil {
		return err
	}
	var listResp CFListResponse
	decErr := json.NewDecoder(respSearch.Body).Decode(&listResp)
	respSearch.Body.Close()
	if decErr != nil {
		return fmt.Errorf("解析 Cloudflare 查询响应失败: %v", decErr)
	}
	if !listResp.Success {
		return fmt.Errorf("查询记录失败: %s", cfErrText(listResp.Errors))
	}

	record := CloudflareDNSRecord{Type: recordType, Name: name, Content: ip, TTL: ttl, Proxied: cfg.Proxied}
	body, _ := json.Marshal(record)
	var apiURL, method string
	if len(listResp.Result) > 0 {
		apiURL = fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records/%s", cfg.ZoneID, listResp.Result[0].ID)
		method = "PUT"
	} else {
		apiURL = fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records", cfg.ZoneID)
		method = "POST"
	}
	req, err := http.NewRequestWithContext(ctx, method, apiURL, bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	setCFAuthHeaders(req, cfg)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var wr struct {
		Success bool      `json:"success"`
		Errors  []CFError `json:"errors"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&wr)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("Cloudflare API 状态码 %d: %s", resp.StatusCode, cfErrText(wr.Errors))
	}
	if !wr.Success {
		return fmt.Errorf("Cloudflare API 返回失败: %s", cfErrText(wr.Errors))
	}
	return nil
}

func ListCFRecords(cfg DNSConfig, name string) ([]CFRecord, error) {
	if err := checkDNSCfg(cfg); err != nil {
		return nil, err
	}
	u := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records?per_page=100", cfg.ZoneID)
	if strings.TrimSpace(name) != "" {
		u += "&name=" + url.QueryEscape(strings.TrimSpace(name))
	}
	client := newOutboundHTTPClient(30 * time.Second)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	setCFAuthHeaders(req, cfg)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var lr CFListResponse
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		return nil, fmt.Errorf("解析 Cloudflare 响应失败: %v", err)
	}
	if !lr.Success {
		return nil, fmt.Errorf("查询失败: %s", cfErrText(lr.Errors))
	}
	return lr.Result, nil
}

func DeleteCFRecord(cfg DNSConfig, id string) error {
	if err := checkDNSCfg(cfg); err != nil {
		return err
	}
	if strings.TrimSpace(id) == "" {
		return errors.New("缺少记录 ID")
	}
	u := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records/%s", cfg.ZoneID, id)
	client := newOutboundHTTPClient(30 * time.Second)
	req, err := http.NewRequest("DELETE", u, nil)
	if err != nil {
		return err
	}
	setCFAuthHeaders(req, cfg)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var wr struct {
		Success bool      `json:"success"`
		Errors  []CFError `json:"errors"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&wr)
	if !wr.Success {
		return fmt.Errorf("删除失败(%d): %s", resp.StatusCode, cfErrText(wr.Errors))
	}
	return nil
}

func setCFAuthHeaders(req *http.Request, cfg DNSConfig) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
}