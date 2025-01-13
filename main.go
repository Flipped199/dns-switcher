package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/BurntSushi/toml"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

const (
	CloudflareAPIURL = "https://api.cloudflare.com/client/v4"
)

// 记录某个监控url所对应的dns是否是备用节点
var isFallback map[string]bool
var failedCount = make(map[string]int)
var c *Config

// Config 配置结构体
type Config struct {
	CloudflareAPIToken string      `toml:"cloudflare_api_token"`
	CloudflareZoneID   string      `toml:"cloudflare_zone_id"`
	CheckInterval      int         `toml:"check_interval"`
	DNSRecords         []DNSRecord `toml:"dns_records"`
}

type DNSRecord struct {
	Type           string           `toml:"type"`
	DNSRecordID    string           `toml:"dns_record_id"`
	HealthCheckUrl string           `toml:"health_check_url"`
	Fallback       DNSRecordDetails `toml:"fallback"`
	DNSRecordDetails
}

type DNSRecordDetails struct {
	Content string `toml:"content"`
	Proxied bool   `toml:"proxied"`
}

type DNSDetailsResp struct {
	Result struct {
		Id        string `json:"id"`
		ZoneId    string `json:"zone_id"`
		ZoneName  string `json:"zone_name"`
		Name      string `json:"name"`
		Type      string `json:"type"`
		Content   string `json:"content"`
		Proxiable bool   `json:"proxiable"`
		Proxied   bool   `json:"proxied"`
		Ttl       int    `json:"ttl"`
		Settings  struct {
		} `json:"settings"`
		Meta struct {
			AutoAdded           bool `json:"auto_added"`
			ManagedByApps       bool `json:"managed_by_apps"`
			ManagedByArgoTunnel bool `json:"managed_by_argo_tunnel"`
		} `json:"meta"`
		Comment    interface{}   `json:"comment"`
		Tags       []interface{} `json:"tags"`
		CreatedOn  time.Time     `json:"created_on"`
		ModifiedOn time.Time     `json:"modified_on"`
	} `json:"result"`
	Success  bool          `json:"success"`
	Errors   []interface{} `json:"errors"`
	Messages []interface{} `json:"messages"`
}

type BatchPatchParams struct {
	Patches []PatchParams `json:"patches"`
}

type PatchParams struct {
	Id      string `json:"id"`
	Content string `json:"content,omitempty"`
	Proxied bool   `json:"proxied"`
}

// 读取配置文件
func loadConfig(filePath string) (*Config, error) {
	config := &Config{}
	if _, err := toml.DecodeFile(filePath, config); err != nil {
		return nil, err
	}
	return config, nil
}

// 构建全局任务 map，key 为 HealthCheckUrl，value 为 []DNSRecord
func buildHealthCheckMap() map[string][]DNSRecord {
	healthCheckMap := make(map[string][]DNSRecord)
	for _, record := range c.DNSRecords {
		if record.HealthCheckUrl != "" {
			healthCheckMap[record.HealthCheckUrl] = append(healthCheckMap[record.HealthCheckUrl], record)
		}
	}
	return healthCheckMap
}

// 检查反代服务器是否可用
func healthCheck(healthCheckUrl string) bool {
	client := http.Client{
		Timeout: 5 * time.Second,
	}

	resp, err := client.Get(healthCheckUrl)
	if err != nil {
		fmt.Printf("Health check failed : %v\n", err)
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return true
	}

	fmt.Printf("Health check failed for URL: %s, status code: %d\n", healthCheckUrl, resp.StatusCode)
	return false
}

// 执行健康检查
func performHealthChecks(healthCheckMap map[string][]DNSRecord) map[string]bool {
	results := make(map[string]bool, len(healthCheckMap))
	var wg sync.WaitGroup
	var resultsMutex sync.Mutex

	for url := range healthCheckMap {
		wg.Add(1)
		go func(url string) {
			defer wg.Done()
			healthy := healthCheck(url)

			// 存储检查结果
			resultsMutex.Lock()
			results[url] = healthy
			resultsMutex.Unlock()
		}(url)
	}

	wg.Wait() // 等待所有检查完成
	return results
}

// 根据健康检查结果更新 DNS 记录
func updateDNSRecords(healthCheckMap map[string][]DNSRecord, healthResults map[string]bool) {
	var data BatchPatchParams

	for url, records := range healthCheckMap {
		var getContentAndProxied func(record DNSRecord) (string, bool)
		shouldUpdate := false

		// 根据健康检查结果和当前状态决定是否更新
		if !healthResults[url] { // 反代不可用
			if !isFallback[url] { // 当前不是 Fallback，更新为 Fallback
				// 进入次数递增，当达到指定次数时，更新dns
				if failedCount[url]+1 < 3 {
					failedCount[url] = failedCount[url] + 1
					fmt.Printf("Failed count %d \n", failedCount[url])
					continue
				}

				getContentAndProxied = func(record DNSRecord) (string, bool) {
					return record.Fallback.Content, record.Fallback.Proxied
				}
				isFallback[url] = true
				shouldUpdate = true
				fmt.Printf("Switching '%s' to Fallback configuration\n", url)
			}
		} else { // 反代可用
			if isFallback[url] { // 当前是 Fallback，切回正常配置
				getContentAndProxied = func(record DNSRecord) (string, bool) {
					return record.Content, record.Proxied
				}
				isFallback[url] = false
				shouldUpdate = true
				fmt.Printf("Switching '%s' to Normal configuration\n", url)
			}
		}

		// 如果无需更新，跳过
		if !shouldUpdate {
			fmt.Printf("No update needed for '%s'\n", url)
			// 归零
			failedCount[url] = 0
			continue
		}

		// 遍历记录并生成批量操作列表
		for _, record := range records {
			content, proxied := getContentAndProxied(record)
			data.Patches = append(data.Patches, PatchParams{
				Id:      record.DNSRecordID,
				Content: content,
				Proxied: proxied,
			})
		}
	}

	// 批量更新 DNS 记录
	if len(data.Patches) > 0 {
		fmt.Println("Updating DNS records ...")
		if err := batchPatchDNSRecords(data); err != nil {
			fmt.Printf("Failed to update DNS records: %v\n", err)
			return
		}
		fmt.Println("Successfully updated DNS records!")
	}
}

// 批量更新dns记录
func batchPatchDNSRecords(data BatchPatchParams) error {
	url := fmt.Sprintf("%s/zones/%s/dns_records/batch", CloudflareAPIURL, c.CloudflareZoneID)

	// 构造请求数据
	body, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("failed to serialize request data: %v", err)
	}

	fmt.Println("Update parameters", string(body))

	resp, err := request(url, http.MethodPost, bytes.NewBuffer(body))
	if err != nil {
		return err
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to update DNS record: %s", resp.Status)
	}

	return nil
}

// 查询dns记录
func getDNSDetails(dnsRecordId string) (*DNSDetailsResp, error) {
	url := fmt.Sprintf("%s/zones/%s/dns_records/%s", CloudflareAPIURL, c.CloudflareZoneID, dnsRecordId)
	resp, err := request(url, http.MethodGet, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var dnsDetails DNSDetailsResp
	if err = json.Unmarshal(bodyBytes, &dnsDetails); err != nil {
		return nil, err
	}
	return &dnsDetails, nil
}

func request(url string, method string, payload io.Reader) (*http.Response, error) {
	// 发送 HTTP PUT 请求
	req, err := http.NewRequest(method, url, payload)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.CloudflareAPIToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %v", err)
	}
	return resp, nil
}

func initDnsStatus(healthCheckMap map[string][]DNSRecord) {
	isFallback = make(map[string]bool, len(healthCheckMap))

	for url, records := range healthCheckMap {
		fallback := true
		for _, record := range records {
			// 查询当前dns解析的目标地址
			details, err := getDNSDetails(record.DNSRecordID)
			if err != nil {
				fmt.Printf("failed to get details for %s: %v\n", record.DNSRecordID, err)
				continue
			}
			if details.Success && details.Result.Content != record.Fallback.Content {
				fallback = false
				break
			}
		}
		isFallback[url] = fallback
	}
}

func main() {
	// 加载配置文件
	configPath := "config.toml"
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}

	config, err := loadConfig(configPath)
	if err != nil {
		fmt.Println("Failed to load config:", err)
		os.Exit(1)
	}
	c = config

	healthCheckMap := buildHealthCheckMap()

	initDnsStatus(healthCheckMap)

	fmt.Println("Starting DNS switcher...")

	for {
		// 执行健康检查
		healthResults := performHealthChecks(healthCheckMap)
		// 根据健康检查结果更新 DNS 记录
		updateDNSRecords(healthCheckMap, healthResults)
		// 等待下一次检查
		time.Sleep(time.Duration(c.CheckInterval) * time.Second)
	}
}
