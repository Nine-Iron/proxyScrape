package main

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// List of proxy sites
var proxySites = []string{
	"https://free-proxy-list.net/",
	"https://www.sslproxies.org/",
	"https://www.us-proxy.org/",
	"https://www.socks-proxy.net/",
	"https://www.proxynova.com/proxy-server-list/",
	"https://hidemy.name/en/proxy-list/",
	"https://spys.one/en/free-proxy-list/",
	"https://www.proxy-list.download/HTTP",
	"https://www.proxy-list.download/SOCKS5",
	"https://proxylist.geonode.com/free-proxy-list",
	"https://www.openproxy.space/list/",
	"https://proxydb.net/",
	"https://www.proxyscrape.com",
}

type ProxyPool struct {
	mu      sync.RWMutex
	proxies []string
	current int
}

func (p *ProxyPool) Add(proxy string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.proxies = append(p.proxies, proxy)
}

func (p *ProxyPool) GetNext() (string, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.proxies) == 0 {
		return "", false
	}
	p.current = (p.current + 1) % len(p.proxies)
	return p.proxies[p.current], true
}

func scrapeProxies(url string, wg *sync.WaitGroup, proxyChan chan<- string) {
	defer wg.Done()

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:       10,
			IdleConnTimeout:    30 * time.Second,
			DisableCompression: true,
			DisableKeepAlives:  true,
		},
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		fmt.Printf("Error creating request: %v\n", err)
		return
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Error fetching %s: %v\n", url, err)
		return
	}
	defer resp.Body.Close()

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return
	}

	// Try table scraping first
	doc.Find("table tbody tr").Each(func(i int, row *goquery.Selection) {
		ip := row.Find("td").Eq(0).Text()
		port := row.Find("td").Eq(1).Text()
		protocol := row.Find("td").Eq(4).Text()

		if ip != "" && port != "" {
			proxy := fmt.Sprintf("%s:%s", ip, port)
			if strings.Contains(strings.ToLower(protocol), "socks5") {
				proxyChan <- "socks5://" + proxy
			} else {
				proxyChan <- "http://" + proxy
			}
		}
	})

	// Try JavaScript deobfuscation
	doc.Find("script").Each(func(_ int, s *goquery.Selection) {
		js := s.Text()
		if strings.Contains(js, "document.write") {
			ip := deobfuscateIP(js)
			if ip != "" {
				proxyChan <- "http://" + ip
			}
		}
	})
}

func deobfuscateIP(js string) string {
	if strings.Contains(js, "atob") {
		re := regexp.MustCompile(`atob\("([^"]+)"\)`)
		if match := re.FindStringSubmatch(js); len(match) > 1 {
			decoded, _ := base64.StdEncoding.DecodeString(match[1])
			return string(decoded)
		}
	}

	js = strings.ReplaceAll(js, "document.write", "")
	js = strings.ReplaceAll(js, "repeat", "")
	js = strings.ReplaceAll(js, "substring", "")
	js = strings.ReplaceAll(js, "concat", "")
	re := regexp.MustCompile(`[\d.]+`)
	return strings.Join(re.FindAllString(js, -1), "")
}
func isValidIP(ip string) bool {
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		return false
	}

	for _, part := range parts {
		num, err := strconv.Atoi(part)
		if err != nil || num < 0 || num > 255 {
			return false
		}
	}
	return true
}
func validateProxy(proxy string) bool {
	parts := strings.Split(strings.TrimPrefix(strings.TrimPrefix(proxy, "http://"), "socks5://"), ":")
	if len(parts) != 2 {
		return false
	}

	ip, port := parts[0], parts[1]
	if !isValidIP(ip) {
		return false
	}

	portNum, err := strconv.Atoi(port)
	if err != nil || portNum < 1 || portNum > 65535 {
		return false
	}

	proxyURL, err := url.Parse(proxy)
	if err != nil {
		return false
	}

	client := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		},
		Timeout: 7 * time.Second,
	}

	resp, err := client.Get("http://api.ipify.org")
	if err != nil {
		fmt.Printf("Dead: %s (error: %v)\n", proxy, err)
		return false
	}
	defer resp.Body.Close()

	fmt.Printf("Alive: %s\n", proxy)
	return resp.StatusCode == 200
}

func saveProxies(filename string, proxies []string) error {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	for _, proxy := range proxies {
		file.WriteString(proxy + "\n")
	}
	fmt.Printf("Saved %d proxies to %s\n", len(proxies), filename)
	return nil
}

func main() {
	pool := &ProxyPool{proxies: make([]string, 0)}
	var wg sync.WaitGroup
	proxyChan := make(chan string, 1000)
	validChan := make(chan string, 1000)

	// Start proxy scrapers
	for _, site := range proxySites {
		wg.Add(1)
		go scrapeProxies(site, &wg, proxyChan)
	}

	// Start validator workers
	const numWorkers = 20
	var validatorWg sync.WaitGroup

	for i := 0; i < numWorkers; i++ {
		validatorWg.Add(1)
		go func() {
			defer validatorWg.Done()
			for proxy := range proxyChan {
				if validateProxy(proxy) {
					validChan <- proxy
				}
			}
		}()
	}

	// Close channels when done
	go func() {
		wg.Wait()
		close(proxyChan)
	}()

	go func() {
		validatorWg.Wait()
		close(validChan)
	}()

	// Collect valid proxies
	var validProxies []string
	for proxy := range validChan {
		pool.Add(proxy)
		validProxies = append(validProxies, proxy)
		fmt.Printf("Valid proxy found: %s\n", proxy)
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
	}
	fileName := filepath.Join(homeDir, ".proxychains", "proxies")
	saveProxies(fileName, validProxies)
	fmt.Printf("\nTotal valid proxies: %d\n", len(validProxies))
}
