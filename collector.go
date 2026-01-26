package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
)

// 全局常量定义
const (
	// promFile: node_exporter 读取 textfile 指标的标准路径
	promFile     = "/var/lib/node_exporter_textfile/custom_metrics.prom"
	// metricPrefix: 给所有自定义指标添加前缀，防止与系统指标冲突
	metricPrefix = "edge_"
	// confPath: 节点配置文件路径，通过 ConfigMap 挂载
	confPath     = "/scripts/nodes.conf"
)

/**
 * WriteToTextfile: 将内存中的指标原子化地写入磁盘
 * 为了防止读取时文件正在写入导致数据受损，采用“临时文件+重命名(Rename)”的原子操作
 */
func WriteToTextfile(path string, gatherer prometheus.Gatherer) error {
	// 创建一个唯一的临时文件名，防止多协程/进程冲突
	tmpPath := fmt.Sprintf("%s.%d.%d", path, os.Getpid(), runtime.NumGoroutine())

	// 函数退出时，确保清理掉临时文件
	defer func() {
		if _, err := os.Stat(tmpPath); err == nil {
			os.Remove(tmpPath)
		}
	}()

	// 1. 创建并打开临时文件
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}

	// 2. 从注册表中收集当前所有指标
	mfs, err := gatherer.Gather()
	if err != nil {
		f.Close()
		return err
	}

	// 3. 使用 Prometheus 标准文本格式编码器写入文件
	enc := expfmt.NewEncoder(f, expfmt.FmtText)
	for _, mf := range mfs {
		if err := enc.Encode(mf); err != nil {
			f.Close()
			return err
		}
	}

	if err := f.Close(); err != nil {
		return err
	}

	// 4. 原子操作：将临时文件重命名为正式文件（旧文件会被替换）
	return os.Rename(tmpPath, path)
}

/**
 * runAndParseScript: 调用外部 Shell 脚本并解析其输出
 * 脚本输出格式预期为: key=value
 */
func runAndParseScript(scriptPath string, nodeName string, nodeType string, registry *prometheus.Registry) error {
	// 设置 10 秒强制超时，防止因硬件阻塞（如 I2C 总线挂死）导致 Sidecar 僵死
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 创建并启动 Bash 进程执行脚本
	cmd := exec.CommandContext(ctx, "/bin/bash", scriptPath)
	stdout, err := cmd.StdoutPipe() // 获取脚本的标准输出流
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	// 逐行读取脚本输出
	scanner := bufio.NewScanner(stdout)
	count := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// 跳过空行、注释行或不含等号的无效行
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}

		// 以第一个等号拆分 key 和 value
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}

		// 对 Key 进行清理并加上前缀，将 Value 转为浮点数
		key := metricPrefix + sanitize(parts[0])
		val, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		if err != nil {
			continue
		}

		// 为每个获取到的 Key 创建一个新的 Prometheus Gauge 指标
		gauge := prometheus.NewGauge(prometheus.GaugeOpts{
			Name: key,
			Help: fmt.Sprintf("Metric from %s", nodeType),
			// 注入静态标签：标识当前数据属于哪个节点
			ConstLabels: prometheus.Labels{
				"node": nodeName,
			},
		})
		
		// 注册指标到本次循环的局部注册表中，并设值
		registry.MustRegister(gauge)
		gauge.Set(val)
		count++
	}

	err = cmd.Wait() // 等待子进程结束
	log.Printf("SUCCESS: Collected %d metrics from %s", count, scriptPath)
	return err
}

/**
 * sanitize: 格式化指标名称
 * Prometheus 指标名只允许 [a-zA-Z_:][a-zA-Z0-9_:]*
 */
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == ':' {
			return r
		}
		return '_' // 非法字符替换为下划线
	}, s)
}

/**
 * parseNodeConfig: 策略中心
 * 解析 nodes.conf，根据当前主机名决定节点的角色(Type)和采集频率(Interval)
 */
func parseNodeConfig(nodeName string) (string, time.Duration) {
	defaultType := "default"
	defaultInterval := 15 * time.Second

	// 打开配置文件
	f, err := os.Open(confPath)
	if err != nil {
		log.Printf("CONFIG: Cannot open %s, using default: %s, %v", confPath, defaultType, defaultInterval)
		return defaultType, defaultInterval
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}

		// 格式: hostname=type:interval
		parts := strings.SplitN(line, "=", 2)
		if strings.TrimSpace(parts[0]) == nodeName {
			val := strings.TrimSpace(parts[1])
			
			// 如果包含冒号，拆分出频率部分
			if strings.Contains(val, ":") {
				configParts := strings.SplitN(val, ":", 2)
				nodeType := configParts[0]
				// time.ParseDuration 支持 "10s", "1m", "2h" 等
				interval, err := time.ParseDuration(configParts[1])
				if err != nil {
					log.Printf("CONFIG: Invalid duration '%s' for node %s, fallback to 15s", configParts[1], nodeName)
					return nodeType, defaultInterval
				}
				return nodeType, interval
			}
			// 仅返回类型
			return val, defaultInterval
		}
	}
	return defaultType, defaultInterval
}

func main() {
	// 从环境变量获取 Kubernetes 注入的当前节点主机名
	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		nodeName = "unknown"
	}

	// 1. 初始化配置解析
	nodeType, interval := parseNodeConfig(nodeName)
	scriptPath := fmt.Sprintf("/scripts/%s.sh", nodeType)
	
	// 在进入循环前，统一打印当前节点的身份和频率配置
	log.Printf("COLLECTOR: Node: %s, Type: %s, Interval: %v, Script: %s", nodeName, nodeType, interval, scriptPath)
	
	// 2. 检查脚本是否存在，或是否为 IDLE (default) 节点
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) || nodeType == "default" {
		log.Printf("IDLE: Node %s (type: %s) enters IDLE mode. Heartbeat: 1h.", nodeName, nodeType)
		// IDLE 模式：不执行任何脚本，仅保持 1 小时一次的心跳，极度节省资源
		for {
			log.Printf("HEARTBEAT: Sidecar is alive on %s (Idle).", nodeName)
			time.Sleep(1 * time.Hour)
		}
	}

	// 3. 核心调度循环：使用 Ticker 按照解析出的间隔精准触发
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// 每次采集创建一个全新的内存注册表，防止指标陈旧（Stale）
			registry := prometheus.NewRegistry()
			
			// 执行 Shell 脚本并解析结果填入注册表
			if err := runAndParseScript(scriptPath, nodeName, nodeType, registry); err != nil {
				log.Printf("ERROR: Collection failed: %v", err)
			}

			// 确保 textfile 目录存在（容器内挂载点）
			os.MkdirAll(filepath.Dir(promFile), 0755)

			// 将采集到的指标写入磁盘，供 node_exporter 抓取
			if err := WriteToTextfile(promFile, registry); err != nil {
				log.Printf("ERROR: Textfile write error: %v", err)
			}
		}
	}
}