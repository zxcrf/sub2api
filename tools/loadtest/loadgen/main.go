// Command loadgen 是闭环压测客户端：固定并发数的 worker 循环发请求，
// 统计吞吐、首字节延迟(TTFB)、总耗时分位数与错误分布，并可同时采样
// 目标进程的 CPU / RSS（通过 /proc）。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type sample struct {
	ttfb, total time.Duration
}

func main() {
	url := flag.String("url", "", "target URL")
	keys := flag.String("keys", "", "comma separated API keys (round-robin per worker)")
	keysFile := flag.String("keys-file", "", "file with one API key per line")
	authStyle := flag.String("auth", "bearer", "bearer | x-api-key")
	conc := flag.Int("c", 100, "concurrent workers")
	dur := flag.Duration("d", 30*time.Second, "test duration")
	warm := flag.Duration("warmup", 5*time.Second, "warmup excluded from stats")
	ramp := flag.Duration("ramp", 3*time.Second, "time over which workers are started")
	bodyFile := flag.String("body", "", "request body file")
	pid := flag.Int("pid", 0, "pid to sample CPU/RSS from")
	label := flag.String("label", "", "label echoed in the JSON result")
	timeout := flag.Duration("timeout", 120*time.Second, "per request timeout")
	flag.Parse()

	body, err := os.ReadFile(*bodyFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read body:", err)
		os.Exit(1)
	}
	var keyList []string
	if *keysFile != "" {
		b, err := os.ReadFile(*keysFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "read keys:", err)
			os.Exit(1)
		}
		keyList = strings.Fields(string(b))
	} else if *keys != "" {
		keyList = strings.Split(*keys, ",")
	}

	tr := &http.Transport{
		MaxIdleConns:        *conc * 2,
		MaxIdleConnsPerHost: *conc * 2,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		DisableCompression:  true,
	}
	client := &http.Client{Transport: tr}

	var (
		mu       sync.Mutex
		samples  []sample
		statuses = map[string]int{}
		okCount  atomic.Int64
		errCount atomic.Int64
		rxBytes  atomic.Int64
		firstErr atomic.Value
	)
	start := time.Now()
	measureFrom := start.Add(*warm)
	deadline := measureFrom.Add(*dur)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	var cpuStart, cpuEnd float64
	var rssPeak atomic.Int64
	var rssSum, rssN int64
	stopSampler := make(chan struct{})
	if *pid > 0 {
		go func() {
			time.Sleep(time.Until(measureFrom))
			cpuStart = procCPUSeconds(*pid)
			t := time.NewTicker(500 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-stopSampler:
					return
				case <-t.C:
					r := procRSSKB(*pid)
					rssSum += r
					rssN++
					if r > rssPeak.Load() {
						rssPeak.Store(r)
					}
				}
			}
		}()
	}

	var wg sync.WaitGroup
	for i := 0; i < *conc; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if *ramp > 0 {
				time.Sleep(time.Duration(int64(*ramp) * int64(i) / int64(*conc)))
			}
			key := ""
			if len(keyList) > 0 {
				key = keyList[i%len(keyList)]
			}
			// 请求体里的 __WORKER__ 占位符按 worker 替换，让每个 worker 成为独立会话；
			// 否则所有请求内容相同，会被粘性会话调度到同一个上游账号。
			body := bytes.ReplaceAll(body, []byte("__WORKER__"), []byte(fmt.Sprintf("w%05d", i)))
			buf := make([]byte, 32*1024)
			local := make([]sample, 0, 1024)
			localStatus := map[string]int{}
			for ctx.Err() == nil {
				rctx, rcancel := context.WithTimeout(ctx, *timeout)
				req, _ := http.NewRequestWithContext(rctx, http.MethodPost, *url, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				if key != "" {
					if *authStyle == "x-api-key" {
						req.Header.Set("x-api-key", key)
						req.Header.Set("anthropic-version", "2023-06-01")
					} else {
						req.Header.Set("Authorization", "Bearer "+key)
					}
				}
				t0 := time.Now()
				resp, err := client.Do(req)
				if err != nil {
					rcancel()
					if ctx.Err() != nil {
						break // 测试结束时被打断的请求不计
					}
					localStatus["neterr"]++
					errCount.Add(1)
					firstErr.CompareAndSwap(nil, err.Error())
					time.Sleep(50 * time.Millisecond)
					continue
				}
				var ttfb time.Duration
				var n int64
				var first []byte
				var rerr error
				for {
					m, e := resp.Body.Read(buf)
					if m > 0 {
						if n == 0 {
							ttfb = time.Since(t0)
							first = append(first, buf[:min(m, 300)]...)
						}
						n += int64(m)
					}
					if e != nil {
						if e != io.EOF {
							rerr = e
						}
						break
					}
				}
				resp.Body.Close()
				rcancel()
				total := time.Since(t0)
				if rerr != nil && ctx.Err() != nil {
					break
				}
				// 按“完成时刻”落在统计窗口内计数；按开始时刻计会漏掉跨窗口边界的
				// 长请求，流式场景下吞吐被明显低估。
				inWindow := time.Now().After(measureFrom)
				code := strconv.Itoa(resp.StatusCode)
				if rerr != nil {
					code = "readerr"
				}
				if inWindow {
					localStatus[code]++
					rxBytes.Add(n)
				}
				if resp.StatusCode == 200 && rerr == nil {
					if inWindow {
						okCount.Add(1)
						local = append(local, sample{ttfb, total})
					}
				} else {
					if inWindow {
						errCount.Add(1)
					}
					msg := code + ": " + string(first)
					if rerr != nil {
						msg = rerr.Error()
					}
					firstErr.CompareAndSwap(nil, msg)
					time.Sleep(50 * time.Millisecond) // 出错时退避，避免空转刷错误
				}
			}
			mu.Lock()
			samples = append(samples, local...)
			for k, v := range localStatus {
				statuses[k] += v
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(measureFrom)
	if *pid > 0 {
		cpuEnd = procCPUSeconds(*pid)
		close(stopSampler)
	}

	ttfbs := make([]time.Duration, len(samples))
	totals := make([]time.Duration, len(samples))
	for i, s := range samples {
		ttfbs[i], totals[i] = s.ttfb, s.total
	}
	sort.Slice(ttfbs, func(i, j int) bool { return ttfbs[i] < ttfbs[j] })
	sort.Slice(totals, func(i, j int) bool { return totals[i] < totals[j] })

	res := map[string]any{
		"label":       *label,
		"concurrency": *conc,
		"duration_s":  round(elapsed.Seconds(), 1),
		"ok":          okCount.Load(),
		"errors":      errCount.Load(),
		"rps":         round(float64(okCount.Load())/elapsed.Seconds(), 1),
		"rx_mb_s":     round(float64(rxBytes.Load())/elapsed.Seconds()/1e6, 2),
		"statuses":    statuses,
		"ttfb_ms":     pct(ttfbs),
		"total_ms":    pct(totals),
	}
	if *pid > 0 {
		cores := (cpuEnd - cpuStart) / elapsed.Seconds()
		res["target_cpu_cores"] = round(cores, 2)
		if okCount.Load() > 0 {
			res["target_cpu_ms_per_req"] = round((cpuEnd-cpuStart)*1000/float64(okCount.Load()), 3)
		}
		res["target_rss_peak_mb"] = rssPeak.Load() / 1024
		if rssN > 0 {
			res["target_rss_avg_mb"] = rssSum / rssN / 1024
		}
	}
	if v := firstErr.Load(); v != nil {
		res["first_error"] = v
	}
	out, _ := json.Marshal(res)
	fmt.Println(string(out))
}

func pct(d []time.Duration) map[string]float64 {
	if len(d) == 0 {
		return nil
	}
	at := func(p float64) float64 {
		i := int(float64(len(d)-1) * p)
		return round(float64(d[i])/1e6, 1)
	}
	return map[string]float64{"p50": at(0.50), "p90": at(0.90), "p99": at(0.99), "max": at(1)}
}

func round(v float64, n int) float64 {
	p := 1.0
	for i := 0; i < n; i++ {
		p *= 10
	}
	return float64(int64(v*p+0.5)) / p
}

// procCPUSeconds 返回进程累计 user+sys CPU 秒数（/proc/<pid>/stat 第 14、15 字段）。
func procCPUSeconds(pid int) float64 {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	s := string(b)
	s = s[strings.LastIndexByte(s, ')')+2:]
	f := strings.Fields(s)
	ut, _ := strconv.ParseFloat(f[11], 64)
	st, _ := strconv.ParseFloat(f[12], 64)
	return (ut + st) / 100 // USER_HZ=100
}

func procRSSKB(pid int) int64 {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			f := strings.Fields(line)
			v, _ := strconv.ParseInt(f[1], 10, 64)
			return v
		}
	}
	return 0
}
