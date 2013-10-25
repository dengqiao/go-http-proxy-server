package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var upstreamMap map[string]*Upstream
var router map[string]*Service
var client *http.Client
var defaultService *Service
var startTime time.Time
var port int

type UpstreamConfig struct {
	Name      string
	Hosts     []string
	FailHosts []string
	CheckUrl  string
}

type ServiceConfig struct {
	UpstreamName string
	PathPrefix   string
	SuccCount    uint64
	FailCount    uint64
	TotoalTime   uint64
	MaxTime      uint64
	MinTime      uint64
}

type Config struct {
	Upstreams []*UpstreamConfig
	Services  []*ServiceConfig
}

type Upstream struct {
	UpstreamConfig
	Lock      *sync.RWMutex
	CheckChan chan bool
}

type Service struct {
	ServiceConfig
	Upstream *Upstream
}

func newUpstream(upstreamConfig *UpstreamConfig) (upstream *Upstream) {
	upstreamConfig.FailHosts = make([]string, 0, len(upstreamConfig.Hosts))
	upstream = &Upstream{
		UpstreamConfig: *upstreamConfig,
		Lock:           &sync.RWMutex{},
		CheckChan:      make(chan bool, len(upstreamConfig.Hosts)),
	}
	upstreamMap[upstreamConfig.Name] = upstream
	go checker(upstream)
	return
}

func newService(serviceConfig *ServiceConfig) {
	service := &Service{ServiceConfig: *serviceConfig}
	upstream, ok := upstreamMap[serviceConfig.UpstreamName]
	if !ok {
		panic("not exist upstream name " + serviceConfig.UpstreamName)
	}
	service.Upstream = upstream
	router[serviceConfig.PathPrefix] = service
}

func init() {
	flag.Usage = func() {
		fmt.Println(usageTemplate)
	}
	flag.IntVar(&port, "port", 8888, "proxy server port.")
	flag.Parse()
	upstreamMap = make(map[string]*Upstream)
	router = make(map[string]*Service)
	loadConfig()

	tr := &http.Transport{
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConnsPerHost:   10,
	}
	client = &http.Client{
		Transport: tr,
	}
	startTime = time.Now()
}

func loadConfig() {
	data, err := ioutil.ReadFile("config.json")
	if err != nil {
		file, err := os.Create("config.json")
		if err != nil {
			flag.Usage()
			panic(err)
		}
		file.Write([]byte(jsonConfigTemplate))
		file.Close()
		fmt.Println("not found config.json,has auto generate for you ,please edit it \n")
		panic(err)
	}
	var config Config
	err = json.Unmarshal(data, &config)
	if err != nil {
		fmt.Println("config.json parse errr\n")
		panic(err)
	}
	for _, upstream := range config.Upstreams {
		newUpstream(upstream)
	}
	for _, service := range config.Services {
		newService(service)
	}
}

func doCheck(upstream *Upstream, failhost string) {
	upstream.Lock.RLock()
	FailHosts := upstream.FailHosts
	upstream.Lock.RUnlock()

	if len(FailHosts) > 0 {
		for _, host := range FailHosts {
			if host == failhost {
				return
			}
		}
	}
	fmt.Printf("host %v offline at %v\n", failhost, time.Now())
	upstream.Lock.Lock()
	newHosts := make([]string, 0)
	for _, succHost := range upstream.Hosts {
		if succHost != failhost {
			newHosts = append(newHosts, succHost)
		}
	}
	upstream.Hosts = newHosts
	upstream.FailHosts = append(upstream.FailHosts, failhost)
	upstream.Lock.Unlock()
	//avoid block
	go func(upstream *Upstream) {
		upstream.CheckChan <- true
	}(upstream)

}

func checker(upstream *Upstream) {
	for {
		upstream.Lock.RLock()
		FailHosts := upstream.FailHosts
		upstream.Lock.RUnlock()
		succHosts := make([]string, 0)
		if len(FailHosts) > 0 {
			for _, host := range FailHosts {
				resp, err := client.Get(host + upstream.CheckUrl)
				if err != nil {

					continue
				}
				ioutil.ReadAll(resp.Body)
				fmt.Printf("alive check status host %v online at %v\n", host, time.Now())
				resp.Body.Close()
				succHosts = append(succHosts, host)
			}
			if len(succHosts) > 0 {
				upstream.Lock.Lock()
				newHosts := make([]string, 0)
				for _, succHost := range succHosts {
					for _, host := range upstream.Hosts {
						if host != succHost {
							newHosts = append(newHosts, host)
						}
					}
					newHosts = append(newHosts, succHost)
				}
				upstream.Hosts = newHosts
				upstream.Lock.Unlock()
			}
		}
		if len(FailHosts) > len(succHosts) {
			//retry check  after 5 second
			time.Sleep(5 * time.Second)
		} else {
			fmt.Printf("upstream %v checker wait\n", upstream.Name)
			_ = <-upstream.CheckChan
			fmt.Printf("upstream %v checker walkup \n", upstream.Name)
		}
	}
}

type proxyHandler struct{}

var hopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te", // canonicalized version of "TE"
	"Trailers",
	"Transfer-Encoding",
	"Upgrade",
}

func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func (_ proxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "/status" {
		serverStatus(w, r)
		return
	}
	service, ok := router[path]
	if ok {
		proxyService(service, w, r)
		return
	}
	subPaths := strings.Split(path, "/")
	i := len(subPaths) - 1
	for i > 1 {
		prefix := strings.Join(subPaths[:i], "/")
		service, ok = router[prefix]
		if ok {
			fmt.Printf("path %v prefix %v index %d\n ", path, prefix, i)
			proxyService(service, w, r)
			return
		}
		i--
	}
	if defaultService != nil {
		proxyService(defaultService, w, r)
		return
	}
	http.Error(w, "proxy err ,no matched rest service for path "+path, 404)
}

//core code from http://godoc.org/net/http/httputil?file=reverseproxy.go#ReverseProxy.ServeHTTP
func proxyService(service *Service, w http.ResponseWriter, r *http.Request) {
	service.Upstream.Lock.RLock()
	host := service.Upstream.Hosts
	if len(host) == 0 {
		atomic.AddUint64(&service.FailCount, 1)
		http.Error(w, "proxy err ,no available service host,please check", 502)
		return
	}
	host0 := host[0]
	if len(host) > 1 {
		index := rand.Int31n(int32(len(host)))
		//fmt.Printf("len %d rand index %d\n", len(host), index)
		host0 = host[index]
	}
	service.Upstream.Lock.RUnlock()
	fmt.Printf("request path " + r.URL.Path + " find host " + host0 + "\n")
	startTime := time.Now().UnixNano()
	var resp *http.Response
	var err error
	rawUrl := host0 + r.URL.Path
	if r.URL.RawQuery != "" {
		rawUrl = rawUrl + "?" + r.URL.RawQuery
	}
	var req *http.Request
	req, err = http.NewRequest(r.Method, rawUrl, r.Body)
	if err != nil {
		http.Error(w, "proxy err "+err.Error(), 502)
		return
	}
	req.Proto = "HTTP/1.1"
	req.ProtoMajor = 1
	req.ProtoMinor = 1
	req.Close = false
	// Remove hop-by-hop headers to the backend.  Especially
	// important is "Connection" because we want a persistent
	// connection, regardless of what the client sent to us.  This
	// is modifying the same underlying map from req (shallow
	// copied above) so we only copy it if necessary.
	copiedHeaders := false
	for _, h := range hopHeaders {
		if req.Header.Get(h) != "" {
			if !copiedHeaders {
				req.Header = make(http.Header)
				copyHeader(req.Header, req.Header)
				copiedHeaders = true
			}
			req.Header.Del(h)
		}
	}
	resp, err = client.Do(req)
	if err != nil {
		fmt.Printf("proxy err: " + err.Error() + " real host is " + host0 + "\n")
		doCheck(service.Upstream, host0)
		//fail over
		proxyService(service, w, r)
		return
	}
	//copy header
	copyHeader(w.Header(), resp.Header)
	//copy StatusCode
	w.WriteHeader(resp.StatusCode)
	//copy content
	io.Copy(w, resp.Body)
	counter(service, true, startTime)
	resp.Body.Close()
	return
}

func serverStatus(w http.ResponseWriter, r *http.Request) {
	upstreamMap := make(map[string]bool)
	upstreams := make([]map[string]interface{}, 0)
	services := make([]map[string]interface{}, 0)
	for _, service := range router {
		_, ok := upstreamMap[service.Upstream.Name]
		if !ok {
			service.Upstream.Lock.RLock()
			upstream := make(map[string]interface{})
			upstream["name"] = service.Upstream.Name
			upstream["failHosts"] = service.Upstream.FailHosts
			upstream["hosts"] = service.Upstream.Hosts
			service.Upstream.Lock.RUnlock()
			upstreams = append(upstreams, upstream)
		}
		SuccCount := atomic.LoadUint64(&service.SuccCount)
		service0 := make(map[string]interface{})
		service0["PathPrefix"] = service.PathPrefix
		service0["UpstreamName"] = service.Upstream.Name
		service0["SuccCount"] = SuccCount
		service0["FailCount"] = atomic.LoadUint64(&service.FailCount)
		service0["MaxTime"] = atomic.LoadUint64(&service.MaxTime)
		service0["MinTime"] = atomic.LoadUint64(&service.MinTime)
		if SuccCount > 0 {
			service0["AvgTime"] = atomic.LoadUint64(&service.TotoalTime) / SuccCount
		}
		services = append(services, service0)
	}
	result := make(map[string]interface{})
	result["upstreams"] = upstreams
	result["services"] = services
	result["startTime"] = startTime
	data, err := json.Marshal(result)
	if err != nil {
		panic(err)
	}
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.Write(data)

}

func counter(service *Service, ok bool, startTime int64) {
	if !ok {
		atomic.AddUint64(&service.FailCount, 1)
		return
	}
	cost := uint64((time.Now().UnixNano() - startTime) / 1000000)
	succCount := atomic.AddUint64(&service.SuccCount, 1)
	totoalTime := atomic.AddUint64(&service.TotoalTime, cost)
	maxTime := atomic.LoadUint64(&service.MaxTime)
	if maxTime < cost {
		atomic.CompareAndSwapUint64(&service.MaxTime, maxTime, cost)
	}
	minTime := atomic.LoadUint64(&service.MinTime)
	if minTime == 0 || minTime > cost {
		atomic.CompareAndSwapUint64(&service.MinTime, minTime, cost)
	}
	fmt.Printf("proxy request ok,cost %v ms,max time %v,min time %v,avg time %v\n",
		cost, maxTime, minTime, totoalTime/succCount)
}

var usageTemplate = `http-proxy-server is a http reverse proxy server write with golang
Usage:

	http-proxy-server --port=8888
	
	current path must exist config.json file, definition proxy rule ,it is json format,For example
` + jsonConfigTemplate
var jsonConfigTemplate = `
{	
	"Upstreams":[
		{
			"Name":"test",
			"Hosts":["http://localhost:8888","http://localhost:8080"],
			"CheckUrl":"/aliveCheck"
		}
	],
	"Services":[
		{
			"UpstreamName":"test",
			"PathPrefix":"/test"
		}
	]
}
`

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU() + 1)
	s := &http.Server{
		Addr:           ":" + strconv.Itoa(port),
		Handler:        proxyHandler{},
		ReadTimeout:    5 * time.Second,
		WriteTimeout:   5 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}
	log.Fatal(s.ListenAndServe())
}
